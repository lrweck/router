package compress

import (
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"testing"
)

type nopRW struct{ h http.Header }

func (n *nopRW) Header() http.Header         { return n.h }
func (n *nopRW) Write(b []byte) (int, error) { return len(b), nil }
func (n *nopRW) WriteHeader(int)             {}

func BenchmarkCompress(b *testing.B) {
	h := New(gzip.DefaultCompression)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello world, this is a small body"))
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rw := &nopRW{h: http.Header{}}
	b.ReportAllocs()
	for b.Loop() {
		h.ServeHTTP(rw, req)
	}
}
