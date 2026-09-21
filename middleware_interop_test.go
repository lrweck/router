package router

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lrweck/router/middleware/nocache"
	"github.com/lrweck/router/middleware/realip"
	"github.com/lrweck/router/middleware/recoverer"
	"github.com/lrweck/router/middleware/requestid"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func interopMiddlewares() []Middleware {
	return []Middleware{requestid.New(), recoverer.New(quietLogger()), realip.New(), nocache.New()}
}

func interopRequest() *http.Request {
	r := httptest.NewRequest("GET", "/users/42", nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r.Header.Set("Forwarded", "for=203.0.113.5")
	return r
}

func checkInterop(t *testing.T, rec *httptest.ResponseRecorder, remote string) {
	t.Helper()
	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d, want 204", rec.Code)
	}
	if rec.Header().Get(requestid.Header) == "" {
		t.Error("request id header missing")
	}
	if rec.Header().Get("Cache-Control") == "" {
		t.Error("nocache header missing")
	}
	if remote != "203.0.113.5:1234" {
		t.Errorf("RemoteAddr = %q, want the Forwarded client", remote)
	}
}

// TestMiddlewareInterop plugs the provided middlewares into both engines through
// the shared Middleware type.
func TestMiddlewareInterop(t *testing.T) {
	t.Run("compat", func(t *testing.T) {
		var remote string
		c := NewRouter()
		c.Use(interopMiddlewares()...)
		c.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
			remote = r.RemoteAddr
			w.WriteHeader(http.StatusNoContent)
		})
		rec := httptest.NewRecorder()
		c.ServeHTTP(rec, interopRequest())
		checkInterop(t, rec, remote)
	})

	t.Run("mux", func(t *testing.T) {
		var remote string
		m := NewMux()
		m.Use(interopMiddlewares()...)
		m.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request, ps Params) {
			remote = r.RemoteAddr
			w.WriteHeader(http.StatusNoContent)
		})
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, interopRequest())
		checkInterop(t, rec, remote)
	})
}

// TestMiddlewareSeesPattern: a root middleware can read the matched pattern
// after next on both engines.
func TestMiddlewareSeesPattern(t *testing.T) {
	capture := func(dst *string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r)
				*dst = r.Pattern
			})
		}
	}
	var compatPattern, muxPattern string

	c := NewRouter()
	c.Use(capture(&compatPattern))
	c.Get("/a/{id}", ok)
	c.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/a/1", nil))

	m := NewMux()
	m.Use(capture(&muxPattern))
	m.Get("/a/{id}", typed)
	m.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/a/1", nil))

	if compatPattern != "/a/{id}" {
		t.Errorf("compat pattern = %q", compatPattern)
	}
	if muxPattern != "/a/{id}" {
		t.Errorf("mux pattern = %q", muxPattern)
	}
}

// TestRecovererThroughRouters: a handler panic becomes a 500 on both engines.
func TestRecovererThroughRouters(t *testing.T) {
	mw := recoverer.New(quietLogger())

	c := NewRouter()
	c.Use(mw)
	c.Get("/boom", func(w http.ResponseWriter, r *http.Request) { panic("x") })
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, httptest.NewRequest("GET", "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("compat panic = %d, want 500", rec.Code)
	}

	m := NewMux()
	m.Use(mw)
	m.Get("/boom", func(w http.ResponseWriter, r *http.Request, ps Params) { panic("x") })
	rec = httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("GET", "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("mux panic = %d, want 500", rec.Code)
	}
}
