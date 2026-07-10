package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedLocal(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		host       string
		origin     string
		forwarded  bool
		want       bool
	}{
		{
			name:       "loopback, no origin header",
			remoteAddr: "127.0.0.1:54321",
			host:       "127.0.0.1:1430",
			want:       true,
		},
		{
			name:       "loopback, origin matches host",
			remoteAddr: "127.0.0.1:54321",
			host:       "127.0.0.1:1430",
			origin:     "http://127.0.0.1:1430",
			want:       true,
		},
		{
			name:       "loopback, origin host mismatch",
			remoteAddr: "127.0.0.1:54321",
			host:       "127.0.0.1:1430",
			origin:     "http://evil.example.com",
			want:       false,
		},
		{
			name:       "loopback, origin port mismatch",
			remoteAddr: "127.0.0.1:54321",
			host:       "127.0.0.1:1430",
			origin:     "http://127.0.0.1:9999",
			want:       false,
		},
		{
			name:       "loopback, origin is the literal null",
			remoteAddr: "127.0.0.1:54321",
			host:       "127.0.0.1:1430",
			origin:     "null",
			want:       false,
		},
		{
			name:       "loopback, malformed origin",
			remoteAddr: "127.0.0.1:54321",
			host:       "127.0.0.1:1430",
			origin:     "http://[::1]:notaport",
			want:       false,
		},
		{
			name:       "loopback, origin matches host but request is forwarded",
			remoteAddr: "127.0.0.1:54321",
			host:       "127.0.0.1:1430",
			origin:     "http://127.0.0.1:1430",
			forwarded:  true,
			want:       false,
		},
		{
			name:       "non-loopback peer",
			remoteAddr: "203.0.113.7:54321",
			host:       "127.0.0.1:1430",
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://"+tt.host+"/api/agent/exec", nil)
			r.RemoteAddr = tt.remoteAddr
			r.Host = tt.host
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if tt.forwarded {
				r.Header.Set("X-Forwarded-For", "198.51.100.9")
			}

			if got := TrustedLocal(r); got != tt.want {
				t.Errorf("TrustedLocal() = %v, want %v", got, tt.want)
			}
		})
	}
}
