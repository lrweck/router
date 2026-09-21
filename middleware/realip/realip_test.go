package realip

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientAddress(t *testing.T) {
	cases := []struct{ name, header, value, want string }{
		{"x-forwarded-for first entry", "X-Forwarded-For", "203.0.113.1, 10.0.0.1", "203.0.113.1:1234"},
		{"x-real-ip", "X-Real-IP", "203.0.113.2", "203.0.113.2:1234"},
		{"ipv6", "CF-Connecting-IP", "2001:db8::1", "2001:db8::1:1234"},
		{"invalid is ignored", "X-Forwarded-For", "not-an-ip", "192.0.2.1:1234"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got string
			h := New()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = "192.0.2.1:1234"
			req.Header.Set(c.header, c.value)
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got != c.want {
				t.Errorf("RemoteAddr = %q, want %q", got, c.want)
			}
		})
	}
}

func TestNoHeadersLeavesAddr(t *testing.T) {
	var got string
	h := New()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "192.0.2.1:1234" {
		t.Errorf("RemoteAddr = %q", got)
	}
}

// TestForwardedRFC7239 pins the RFC 7239 parsing, including quoted nodes,
// IPv6 brackets and obfuscated identifiers.
func TestForwardedRFC7239(t *testing.T) {
	cases := []struct {
		name, forwarded, xff, want string
	}{
		{"plain ip", "for=203.0.113.9", "", "203.0.113.9:1234"},
		{"quoted ip:port", `for="203.0.113.9:8080"`, "", "203.0.113.9:1234"},
		{"ipv6 bracket", `for="[2001:db8::1]:8080"`, "", "2001:db8::1:1234"},
		{"first element wins", "for=203.0.113.9;proto=https, for=10.0.0.1", "", "203.0.113.9:1234"},
		{"beats x-forwarded-for", "for=203.0.113.9", "10.0.0.1", "203.0.113.9:1234"},
		{"obfuscated falls back", "for=_hidden", "", "192.0.2.1:1234"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got string
			h := New()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.RemoteAddr }))
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = "192.0.2.1:1234"
			req.Header.Set("Forwarded", c.forwarded)
			if c.xff != "" {
				req.Header.Set("X-Forwarded-For", c.xff)
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			if got != c.want {
				t.Errorf("RemoteAddr = %q, want %q", got, c.want)
			}
		})
	}
}
