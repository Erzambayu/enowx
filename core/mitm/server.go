package mitm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Server is the local MITM TLS proxy. It listens on :443, terminates TLS for the
// redirected IDE hosts with CA-signed leaf certs, and either forwards chat
// requests to the gateway or passes everything else through to the real upstream.
type Server struct {
	ca         *CA
	gatewayURL string // e.g. http://127.0.0.1:1430
	apiKey     string
	model      func(tool, ideModel string) string // resolves the mapped gateway model

	srv  *http.Server
	mu   sync.Mutex
	live bool
}

// NewServer builds the proxy. model maps an IDE's model name to a gateway model
// (returns "" to pass the request through untouched).
func NewServer(ca *CA, gatewayURL, apiKey string, model func(tool, ideModel string) string) *Server {
	return &Server{ca: ca, gatewayURL: strings.TrimRight(gatewayURL, "/"), apiKey: apiKey, model: model}
}

// realDialer resolves upstreams via public DNS (8.8.8.8), bypassing our own
// hosts-file poisoning so forwarding the untouched requests doesn't loop back.
var realDialer = &net.Dialer{
	Timeout: 15 * time.Second,
	Resolver: &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "udp", "8.8.8.8:53")
		},
	},
}

// Start begins listening on :443. Non-blocking; returns once the listener is up.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live {
		return nil
	}
	ln, err := tls.Listen("tcp", ":443", &tls.Config{GetCertificate: s.ca.GetCertificate})
	if err != nil {
		if isPermErr(err) {
			return fmt.Errorf("can't bind port 443 — restart enx with admin/root privileges to run the MITM proxy")
		}
		return fmt.Errorf("listen :443: %w", err)
	}
	s.srv = &http.Server{Handler: http.HandlerFunc(s.handle), ReadHeaderTimeout: 15 * time.Second}
	s.live = true
	go func() { _ = s.srv.Serve(ln) }()
	return nil
}

// Stop shuts the proxy down.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
		s.srv = nil
	}
	s.live = false
}

// Live reports whether the proxy is running.
func (s *Server) Live() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

// handle is the request entrypoint: health check, intercept, or passthrough.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_mitm_health" {
		w.Write([]byte("ok"))
		return
	}
	// Break loops: our own forwarded traffic carries this marker.
	if r.Header.Get("x-enx-mitm") == "1" {
		s.passthrough(w, r)
		return
	}
	tool, ok := toolForHost(r.Host)
	if !ok || !tool.interceptable(r.URL.Path) {
		s.passthrough(w, r)
		return
	}
	s.intercept(w, r, tool)
}

// intercept reads the IDE request, maps the model, forwards to the gateway, and
// streams the reply back in the format the IDE expects. On any failure it falls
// back to a straight passthrough so the IDE keeps working.
func (s *Server) intercept(w http.ResponseWriter, r *http.Request, tool Tool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 20<<20))
	if err != nil {
		s.passthrough(w, r)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		s.passthroughBody(w, r, body)
		return
	}
	ideModel, _ := payload["model"].(string)
	mapped := s.model(tool.Key, ideModel)
	if mapped == "" {
		// No mapping configured → don't touch it, forward to the real upstream.
		s.passthroughBody(w, r, body)
		return
	}
	payload["model"] = mapped
	out, _ := json.Marshal(payload)

	// The gateway is OpenAI/Anthropic/Gemini-compatible via convert.Inbound, so we
	// POST the (model-swapped) body to the matching endpoint and pipe the reply.
	endpoint := s.gatewayURL + gatewayPath(tool.Format)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(out))
	if err != nil {
		s.passthroughBody(w, r, body)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	resp, err := gatewayClient.Do(req)
	if err != nil {
		s.passthroughBody(w, r, body)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flushCopy(w, resp.Body)
}

// gatewayPath maps a tool's wire format to the gateway endpoint that accepts it.
func gatewayPath(format string) string {
	switch format {
	case "gemini":
		// The gateway's OpenAI endpoint accepts converted inbound formats; a Gemini
		// body is normalized by convert.Inbound on the gateway side.
		return "/v1/chat/completions"
	default:
		return "/v1/chat/completions"
	}
}

// passthrough forwards the request untouched to the real upstream.
func (s *Server) passthrough(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 20<<20))
	s.passthroughBody(w, r, body)
}

func (s *Server) passthroughBody(w http.ResponseWriter, r *http.Request, body []byte) {
	url := "https://" + r.Host + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	req.Header = r.Header.Clone()
	req.Header.Set("x-enx-mitm", "1")
	resp, err := passthroughClient.Do(req)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flushCopy(w, resp.Body)
}

// passthroughClient reaches the REAL upstreams via public DNS (not our poisoned
// hosts file).
var passthroughClient = &http.Client{
	Timeout: 5 * time.Minute,
	Transport: &http.Transport{
		DialContext: realDialer.DialContext,
	},
}

var gatewayClient = &http.Client{Timeout: 5 * time.Minute}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Transfer-Encoding") {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// flushCopy streams the body, flushing after each chunk so SSE reaches the IDE
// promptly.
func flushCopy(w http.ResponseWriter, src io.Reader) {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// isPermErr reports whether an error is a privilege/permission failure (binding a
// low port without root).
func isPermErr(err error) bool {
	return errors.Is(err, os.ErrPermission) || strings.Contains(strings.ToLower(err.Error()), "permission denied")
}
