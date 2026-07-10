package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/enowdev/enowx/store"
)

const (
	passwordKey   = "dashboard_password_hash"
	sessionCookie = "enx_session"
	sessionTTL    = 30 * 24 * time.Hour
)

// Dashboard guards the management dashboard (UI + loopback-only tools like the
// terminal and file browser) with an optional password.
//
// Policy: localhost is always trusted (local-first — no login needed). A remote
// request (e.g. through a public tunnel) must carry a valid session cookie, and
// a session can only exist once a password has been set. So remote access to
// the shell/files is impossible until the user sets a password and logs in.
type Dashboard struct {
	settings store.SettingsStore
	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry (cache of the persisted set)
	loaded   bool
}

func NewDashboard(settings store.SettingsStore) *Dashboard {
	return &Dashboard{settings: settings, sessions: map[string]time.Time{}}
}

const sessionsKey = "dashboard_sessions"

// loadLocked hydrates the session cache from the store once (sessions survive
// restarts — they're persisted as JSON in the settings KV).
func (d *Dashboard) loadLocked() {
	if d.loaded {
		return
	}
	d.loaded = true
	raw, _ := d.settings.Get(context.Background(), sessionsKey)
	if raw == "" {
		return
	}
	var stored map[string]int64 // token -> unix expiry
	if json.Unmarshal([]byte(raw), &stored) != nil {
		return
	}
	now := time.Now()
	for tok, exp := range stored {
		t := time.Unix(exp, 0)
		if t.After(now) {
			d.sessions[tok] = t
		}
	}
}

// persistLocked writes the current (non-expired) sessions back to the store.
func (d *Dashboard) persistLocked() {
	out := make(map[string]int64, len(d.sessions))
	for tok, exp := range d.sessions {
		out[tok] = exp.Unix()
	}
	b, _ := json.Marshal(out)
	_ = d.settings.Set(context.Background(), sessionsKey, string(b))
}

// HasPassword reports whether a dashboard password has been set.
func (d *Dashboard) HasPassword(ctx context.Context) bool {
	h, _ := d.settings.Get(ctx, passwordKey)
	return h != ""
}

// SetPassword stores a bcrypt hash of the password.
func (d *Dashboard) SetPassword(ctx context.Context, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return d.settings.Set(ctx, passwordKey, string(hash))
}

// CheckPassword verifies a password against the stored hash.
func (d *Dashboard) CheckPassword(ctx context.Context, password string) bool {
	h, _ := d.settings.Get(ctx, passwordKey)
	if h == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(h), []byte(password)) == nil
}

// CreateSession mints a session token (call after a successful login). The
// session is persisted so it survives a restart.
func (d *Dashboard) CreateSession() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	d.mu.Lock()
	d.loadLocked()
	d.sessions[token] = time.Now().Add(sessionTTL)
	d.persistLocked()
	d.mu.Unlock()
	return token
}

func (d *Dashboard) validSession(token string) bool {
	if token == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loadLocked()
	exp, ok := d.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(d.sessions, token)
		d.persistLocked()
		return false
	}
	return true
}

func (d *Dashboard) revoke(token string) {
	d.mu.Lock()
	d.loadLocked()
	delete(d.sessions, token)
	d.persistLocked()
	d.mu.Unlock()
}

// SetCookie writes the session cookie. Secure is set when the request arrived
// over TLS (e.g. through the tunnel).
func (d *Dashboard) SetCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isTLS(r),
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// ClearCookie + revoke (logout).
func (d *Dashboard) ClearCookie(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		d.revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
}

// Authorized reports whether the request may access the dashboard + its
// loopback tools: trusted if it genuinely comes from the same machine (a
// loopback TCP peer AND not forwarded through the tunnel), or carries a valid
// session. Used both by the Require middleware and directly by the
// terminal/files/agent handlers.
func (d *Dashboard) Authorized(r *http.Request) bool {
	if TrustedLocal(r) {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return d.validSession(c.Value)
}

// bootstrapPaths stay reachable without auth so a first-time REMOTE user can set
// and enter the dashboard password (otherwise remote access would be impossible
// to bootstrap). Everything else under /api requires TrustedLocal or a session.
var bootstrapPaths = map[string]bool{
	"/api/auth/status": true,
	"/api/auth/setup":  true,
	"/api/auth/login":  true,
}

// Require is middleware that 401s any request that isn't Authorized (trusted
// local or session), except the login-bootstrap endpoints. Wrap sensitive route
// groups (e.g. all of /api) with it.
func (d *Dashboard) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bootstrapPaths[r.URL.Path] || d.Authorized(r) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "dashboard login required (reachable locally, or after signing in)", http.StatusUnauthorized)
	})
}

// LoggedIn reports whether the request carries a valid session (ignores
// loopback). Used by the auth status endpoint.
func (d *Dashboard) LoggedIn(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return d.validSession(c.Value)
}

func isTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}

// peerIsLoopback reports whether the request's actual TCP peer is a loopback
// address. Unlike the Host header, RemoteAddr is set by the server from the
// real connection and cannot be spoofed by the client.
func peerIsLoopback(r *http.Request) bool {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// looksForwarded reports whether the request arrived through a reverse proxy /
// tunnel (cloudflared sets these). Such a request reaches the origin from a
// loopback connector, so the TCP peer looks local even though the real client
// is remote — we must NOT trust it as same-machine.
func looksForwarded(r *http.Request) bool {
	h := r.Header
	return h.Get("X-Forwarded-For") != "" ||
		h.Get("X-Forwarded-Proto") != "" ||
		h.Get("X-Forwarded-Host") != "" ||
		h.Get("Cf-Connecting-Ip") != "" ||
		h.Get("Cf-Ray") != ""
}

// originTrusted reports whether a request's Origin header (if any) matches its
// own Host. A browser cannot forge Origin, so a present-but-mismatched Origin
// means the request was sent cross-site by a page running in the user's
// browser (drive-by CSRF) even though the TCP connection itself is loopback.
// Non-browser callers (curl, the enx CLI, local scripts) never send an Origin
// header at all, so their requests are unaffected.
func originTrusted(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// TrustedLocal reports whether the request genuinely originates on this machine:
// a loopback TCP peer that did NOT arrive via the tunnel/proxy, and — if the
// request carries an Origin header at all — one that matches this request's
// own Host. Remote requests (including anything through cloudflared) and
// cross-site browser requests both return false and must authenticate.
func TrustedLocal(r *http.Request) bool {
	return peerIsLoopback(r) && !looksForwarded(r) && originTrusted(r)
}
