package throttle

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type nopRW struct{ h http.Header }

func (n *nopRW) Header() http.Header         { return n.h }
func (n *nopRW) Write(b []byte) (int, error) { return len(b), nil }
func (n *nopRW) WriteHeader(int)             {}

func BenchmarkThrottle(b *testing.B) {
	h := New(8)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest("GET", "/", nil)
	rw := &nopRW{h: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		h.ServeHTTP(rw, req)
	}
}
