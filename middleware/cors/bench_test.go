package cors

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type nopRW struct{ h http.Header }

func (n *nopRW) Header() http.Header         { return n.h }
func (n *nopRW) Write(b []byte) (int, error) { return len(b), nil }
func (n *nopRW) WriteHeader(int)             {}

func BenchmarkCORS(b *testing.B) {
	h := New(Options{AllowedOrigins: []string{"https://ok.example"}})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://ok.example")
	rw := &nopRW{h: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		h.ServeHTTP(rw, req)
	}
}
