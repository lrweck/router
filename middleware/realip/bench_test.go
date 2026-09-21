package realip

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type nopRW struct{ h http.Header }

func (n *nopRW) Header() http.Header         { return n.h }
func (n *nopRW) Write(b []byte) (int, error) { return len(b), nil }
func (n *nopRW) WriteHeader(int)             {}

func BenchmarkRealIP(b *testing.B) {
	h := New()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Forwarded", "for=203.0.113.5")
	rw := &nopRW{h: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		h.ServeHTTP(rw, req)
	}
}
