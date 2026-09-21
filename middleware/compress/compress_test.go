package compress

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func serve(t *testing.T, ae string, status int, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := New(gzip.DefaultCompression)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
		}
		if body != "" {
			io.WriteString(w, body)
		}
	}))
	req := httptest.NewRequest("GET", "/", nil)
	if ae != "" {
		req.Header.Set("Accept-Encoding", ae)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCompresses(t *testing.T) {
	rec := serve(t, "gzip", 0, "hello world")
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", rec.Header().Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != "hello world" {
		t.Errorf("body = %q", got)
	}
}

func TestNoCompressWithoutAccept(t *testing.T) {
	rec := serve(t, "", 0, "hello world")
	if rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("compressed without Accept-Encoding")
	}
	if rec.Body.String() != "hello world" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestQZeroDoesNotCompress(t *testing.T) {
	if rec := serve(t, "gzip;q=0", 0, "hello world"); rec.Header().Get("Content-Encoding") != "" {
		t.Error("gzip;q=0 should not compress")
	}
}

func TestNoBodyStatus(t *testing.T) {
	if rec := serve(t, "gzip", http.StatusNoContent, ""); rec.Header().Get("Content-Encoding") != "" {
		t.Error("204 should not be compressed")
	}
}

func TestKeepsExistingEncoding(t *testing.T) {
	h := New(gzip.DefaultCompression)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		io.WriteString(w, "x")
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "br" {
		t.Errorf("Content-Encoding = %q, want br", got)
	}
}

// TestAcceptEncodingRFC9110 pins the Accept-Encoding parsing to RFC 9110
// §12.5.3 (codings, "*", and qvalues per §12.4.2).
func TestAcceptEncodingRFC9110(t *testing.T) {
	cases := []struct {
		ae   string
		want bool
	}{
		{"gzip", true},
		{"gzip, deflate, br", true},
		{"gzip;q=0", false},
		{"gzip;q=0.000", false},
		{"gzip;q=0.5", true},
		{"gzip;q=1", true},
		{"gzip;q=1.000", true},
		{"*", true},
		{"*;q=0", false},
		{"br, *;q=0.8", true},
		{"br", false},
		{"identity", false},
		{"", false},
	}
	for _, c := range cases {
		if got := acceptsGzip(c.ae); got != c.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", c.ae, got, c.want)
		}
	}
}
