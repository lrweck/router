package router

// These tests pin the cases where Chi and net/http.ServeMux disagree: by
// design the router follows the stdlib. Each test states Chi's behavior in a
// comment so the divergence is explicit and intentional, not accidental.

import (
	"fmt"
	"net/http"
	"testing"
)

// TestStdlibPathCleaning: the stdlib cleans "//" and "." segments with a 301.
// Chi routes "/users///c" to /users/{x}/{y}/{z} with empty params.
func TestStdlibPathCleaning(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{x}/{y}/{z}", func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte("ok"))
	})
	w := do(t, r, "GET", "/users///c")
	if w.Code != http.StatusTemporaryRedirect {
		t.Fatalf("got %d, want 307 (stdlib canonicalization)", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/users/c" {
		t.Fatalf("Location = %q", loc)
	}
}

// TestStdlibTrailingSlashRedirect: with only "/x/" registered, "/x" gets a 301
// from the stdlib; Chi answers 404.
func TestStdlibTrailingSlashRedirect(t *testing.T) {
	r := NewRouter()
	r.Get("/articles/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("list")) })
	w := do(t, r, "GET", "/articles")
	if w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != "/articles/" {
		t.Fatalf("got %d %q, want 307 -> /articles/", w.Code, w.Header().Get("Location"))
	}
	if got := do(t, r, "GET", "/articles/").Body.String(); got != "list" {
		t.Fatalf("trailing = %q", got)
	}
}

// TestStdlibEscapedParams: the stdlib matches on the escaped path but hands
// handlers unescaped values. Chi returns the raw escaped segment.
func TestStdlibEscapedParams(t *testing.T) {
	r := NewRouter()
	r.Get("/api/{id}/x", func(w http.ResponseWriter, req *http.Request) {
		w.Write([]byte(URLParam(req, "id")))
	})
	got := do(t, r, "GET", "/api/a%2Fb/x").Body.String()
	if got != "a/b" {
		t.Fatalf("escaped param = %q, want stdlib-unquoted %q", got, "a/b")
	}
}

// TestStdlibAllowFormat pins the 405 Allow header to net/http's single
// comma-joined value (Chi emits one header per method).
func TestStdlibAllowFormat(t *testing.T) {
	r := NewRouter()
	r.Get("/x", ok)
	r.Post("/x", ok)
	w := do(t, r, "DELETE", "/x")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d", w.Code)
	}
	// HEAD is implied by GET, exactly as net/http reports it.
	allow := w.Header().Values("Allow")
	if len(allow) != 1 || allow[0] != "GET, HEAD, POST" {
		t.Fatalf("Allow = %#v, want single %q", allow, "GET, HEAD, POST")
	}
}

// TestStdlibHeadFromGet: the stdlib serves HEAD from GET routes; Chi needs an
// explicit Head registration or its GetHead middleware.
func TestStdlibHeadFromGet(t *testing.T) {
	r := NewRouter()
	r.Get("/h", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Get", "1")
		w.Write([]byte("body"))
	})
	w := do(t, r, "HEAD", "/h")
	if w.Code != http.StatusOK || w.Header().Get("X-Get") != "1" {
		t.Fatalf("HEAD from GET = %d, X-Get=%q", w.Code, w.Header().Get("X-Get"))
	}
}

// TestStdlibNotFoundBody: the default 404 body/headers come from
// net/http.NotFound, untouched.
func TestStdlibNotFoundBody(t *testing.T) {
	r := NewRouter()
	r.Get("/x", ok)
	w := do(t, r, "GET", "/missing")
	if w.Code != http.StatusNotFound || w.Body.String() != "404 page not found\n" {
		t.Fatalf("404 = %d %q", w.Code, w.Body.String())
	}
}

// TestMissFilterAndCatchall: the least-specific "/" catch-all plus the
// first-segment negative filter must not change 404/405 outcomes.
func TestMissFilterAndCatchall(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id:[0-9]+}", body("u"))
	r.Post("/articles/{slug:[a-z-]+}", body("a"))

	if got := do(t, r, "GET", "/nope/nothing"); got.Code != 404 {
		t.Errorf("unknown first segment = %d, want 404", got.Code)
	}
	if got := do(t, r, "GET", "/users/42").Body.String(); got != "u" {
		t.Errorf("hit = %q", got)
	}
	w := do(t, r, "DELETE", "/users/42")
	if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("405 = %d Allow=%q", w.Code, w.Header().Get("Allow"))
	}
	// Known first segment but failing constraint: 404 (not the catch-all's 200).
	if got := do(t, r, "GET", "/users/abc"); got.Code != 404 {
		t.Errorf("constraint miss = %d, want 404", got.Code)
	}

	// A root-level param disables the filter, but 404s must still work.
	r2 := NewRouter()
	r2.Get("/{x}/y", body("xy"))
	if got := do(t, r2, "GET", "/a/y").Body.String(); got != "xy" {
		t.Errorf("root param hit = %q", got)
	}
	if got := do(t, r2, "GET", "/a/z"); got.Code != 404 {
		t.Errorf("root param miss = %d, want 404", got.Code)
	}
	if got := do(t, r2, "GET", "/a/b/c"); got.Code != 404 {
		t.Errorf("too deep = %d, want 404", got.Code)
	}
}

// TestLazyContext: plain routes carry no routing Context (fast path); routes
// that need it (renamed/mixed params, wildcards, mounts) do.
func TestLazyContext(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id}", func(w http.ResponseWriter, req *http.Request) {
		// Native placeholder name: URLParam reads it via r.PathValue, and no
		// routing Context was allocated.
		if RouteContext(req.Context()) != nil {
			t.Errorf("plain route should not allocate a routing Context")
		}
		if req.Pattern != "/users/{id}" {
			t.Errorf("req.Pattern = %q", req.Pattern)
		}
		fmt.Fprint(w, URLParam(req, "id"))
	})
	if got := do(t, r, "GET", "/users/42").Body.String(); got != "42" {
		t.Errorf("plain param = %q", got)
	}

	r2 := NewRouter()
	r2.Get("/a/{foo-bar}", func(w http.ResponseWriter, req *http.Request) {
		if RouteContext(req.Context()) == nil {
			t.Errorf("renamed param needs a routing Context")
		}
		fmt.Fprint(w, URLParam(req, "foo-bar"))
	})
	if got := do(t, r2, "GET", "/a/x1").Body.String(); got != "x1" {
		t.Errorf("renamed param = %q", got)
	}
}

// TestConstraintDriven405Allow: when a constraint (which the stdlib can't see)
// rejects the request, our own 405 must still emit the stdlib's Allow format:
// sorted, deduped, with HEAD implied by GET.
func TestConstraintDriven405Allow(t *testing.T) {
	r := NewRouter()
	r.Post("/x/{id:[0-9]+}", ok) // registered before the GET on purpose
	r.Put("/x/{id:[0-9]+}", ok)
	r.Get("/x/{id:[a-z]+}", ok) // rejects digits, so the GET slot is empty

	w := do(t, r, "GET", "/x/123")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "POST, PUT" {
		t.Fatalf("Allow = %q, want sorted %q", got, "POST, PUT")
	}

	// GET allowed (so HEAD is implied) while the request's own method is
	// rejected by its constraint.
	r2 := NewRouter()
	r2.Get("/w/{id:[0-9]+}", ok)
	r2.Put("/w/{id:[a-z]+}", ok)
	w2 := do(t, r2, "PUT", "/w/123")
	if w2.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code = %d, want 405", w2.Code)
	}
	if got := w2.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q, want %q", got, "GET, HEAD")
	}
}
