package cors

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func ok(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }

func TestSimpleRequest(t *testing.T) {
	h := New(Options{AllowedOrigins: []string{"https://ok.example"}})(http.HandlerFunc(ok))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://ok.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://ok.example" {
		t.Errorf("Access-Control-Allow-Origin = %q", got)
	}
	if rec.Header().Get("Vary") != "Origin" {
		t.Errorf("Vary = %q", rec.Header().Get("Vary"))
	}
}

func TestDisallowedOriginPassesThrough(t *testing.T) {
	h := New(Options{AllowedOrigins: []string{"https://ok.example"}})(http.HandlerFunc(ok))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("Access-Control-Allow-Origin set for a disallowed origin")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestPreflight(t *testing.T) {
	h := New(Options{AllowedOrigins: []string{"*"}, MaxAge: time.Minute})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("preflight must not reach the handler")
	}))
	req := httptest.NewRequest("OPTIONS", "/", nil)
	req.Header.Set("Origin", "https://x.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "X-Custom")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204", rec.Code)
	}
	for k, want := range map[string]string{
		"Access-Control-Allow-Origin":  "*",
		"Access-Control-Allow-Headers": "X-Custom",
		"Access-Control-Max-Age":       "60",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if rec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("Access-Control-Allow-Methods is empty")
	}
	vary := strings.Join(rec.Header().Values("Vary"), ", ")
	for _, want := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
		if !strings.Contains(vary, want) {
			t.Errorf("Vary = %q, missing %q", vary, want)
		}
	}
}

func TestCredentialsEchoesOrigin(t *testing.T) {
	h := New(Options{AllowedOrigins: []string{"*"}, AllowCredentials: true})(http.HandlerFunc(ok))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://x.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://x.example" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the origin echoed", got)
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Error("Access-Control-Allow-Credentials missing")
	}
}
