package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func typed(w http.ResponseWriter, _ *http.Request, _ Params) { w.WriteHeader(http.StatusNoContent) }

func typedEcho(key string) TypedHandler {
	return func(w http.ResponseWriter, _ *http.Request, ps Params) {
		fmt.Fprint(w, ps.Get(key))
	}
}

// TestMuxRouting covers the typed engine's match shapes.
func TestMuxRouting(t *testing.T) {
	m := NewMux()
	m.Get("/ping", func(w http.ResponseWriter, _ *http.Request, _ Params) { fmt.Fprint(w, "pong") })
	m.Get("/users/{id}", typedEcho("id"))
	m.Get("/d/{m:[0-9]+}-{d:[0-9]+}", func(w http.ResponseWriter, _ *http.Request, ps Params) {
		fmt.Fprintf(w, "%s/%s", ps.Get("m"), ps.Get("d"))
	})
	m.Get("/files/*", func(w http.ResponseWriter, _ *http.Request, ps Params) {
		fmt.Fprint(w, "rest:"+ps.Get("*"))
	})

	cases := []struct {
		method, path, want string
		code               int
	}{
		{"GET", "/ping", "pong", 200},
		{"GET", "/users/42", "42", 200},
		{"GET", "/d/09-21", "09/21", 200},
		{"GET", "/d/x-21", "", 404},
		{"GET", "/files/a/b/c", "rest:a/b/c", 200},
		{"GET", "/files/", "rest:", 200},
		{"GET", "/nope", "", 404},
		{"GET", "/users/42/extra", "", 404},
	}
	for _, c := range cases {
		w := do(t, m, c.method, c.path)
		if w.Code != c.code || (c.want != "" && w.Body.String() != c.want) {
			t.Errorf("%s %s = %d %q, want %d %q", c.method, c.path, w.Code, w.Body.String(), c.code, c.want)
		}
	}
}

// TestMuxStaticPrefixCollision: a static child must match on a segment
// boundary, not just a byte prefix ("user" must not match "/users/1").
func TestMuxStaticPrefixCollision(t *testing.T) {
	m := NewMux()
	m.Get("/user", func(w http.ResponseWriter, _ *http.Request, _ Params) { fmt.Fprint(w, "user") })
	m.Get("/users/{id}", typedEcho("id"))
	m.Get("/a", func(w http.ResponseWriter, _ *http.Request, _ Params) { fmt.Fprint(w, "a") })
	m.Get("/ab", func(w http.ResponseWriter, _ *http.Request, _ Params) { fmt.Fprint(w, "ab") })
	m.Get("/a/{x}", typedEcho("x"))

	must := func(path, want string) {
		t.Helper()
		if got := do(t, m, "GET", path).Body.String(); got != want {
			t.Errorf("GET %s = %q, want %q", path, got, want)
		}
	}
	must("/user", "user")
	must("/users/1", "1")
	must("/a", "a")
	must("/ab", "ab")
	must("/a/z", "z")
	if got := do(t, m, "GET", "/abz").Code; got != 404 {
		t.Errorf("/abz = %d, want 404", got)
	}
}

// TestMuxHighFanout exercises the map promotion path (>4 static children).
func TestMuxHighFanout(t *testing.T) {
	m := NewMux()
	for i := range 12 {
		p := fmt.Sprintf("/r%d/{id}", i)
		want := fmt.Sprintf("r%d", i)
		m.Get(p, func(w http.ResponseWriter, _ *http.Request, _ Params) { fmt.Fprint(w, want) })
	}
	for i := range 12 {
		p := fmt.Sprintf("/r%d/9", i)
		want := fmt.Sprintf("r%d", i)
		if got := do(t, m, "GET", p).Body.String(); got != want {
			t.Errorf("GET %s = %q, want %q", p, got, want)
		}
	}
	if got := do(t, m, "GET", "/r12/9").Code; got != 404 {
		t.Errorf("/r12/9 = %d, want 404", got)
	}
}

// TestMuxBucketAtPathEnd: a path that ends exactly at a node carrying the
// first-byte bucket index reaches childAt with start == len(path); it must miss
// cleanly instead of reading past the path.
func TestMuxBucketAtPathEnd(t *testing.T) {
	m := NewMux()
	for i := range 12 {
		m.Get(fmt.Sprintf("/a/x%d", i), typed)
		m.Get(fmt.Sprintf("/r%d", i), typed)
	}
	for _, p := range []string{"/", "/a", "/a/", "/r0/", "/nope/"} {
		if got := do(t, m, "GET", p).Code; got != 404 {
			t.Errorf("GET %s = %d, want 404", p, got)
		}
	}
	if got := do(t, m, "GET", "/a/x3").Code; got != 204 {
		t.Errorf("GET /a/x3 = %d, want 204", got)
	}
}

// TestMuxMethods: verbs, HEAD-from-GET, 405 Allow, and custom methods.
func TestMuxMethods(t *testing.T) {
	m := NewMux()
	m.Get("/x", typed)
	m.Post("/x", typed)
	m.Handle("PURGE", "/p", typed)

	if got := do(t, m, "GET", "/x").Code; got != 204 {
		t.Errorf("GET = %d", got)
	}
	if got := do(t, m, "POST", "/x").Code; got != 204 {
		t.Errorf("POST = %d", got)
	}
	if got := do(t, m, "HEAD", "/x").Code; got != 204 {
		t.Errorf("HEAD from GET = %d", got)
	}
	if got := do(t, m, "PURGE", "/p").Code; got != 204 {
		t.Errorf("custom PURGE = %d", got)
	}
	w := do(t, m, "DELETE", "/x")
	if w.Code != 405 {
		t.Fatalf("DELETE = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD, POST" {
		t.Errorf("Allow = %q, want sorted %q", got, "GET, HEAD, POST")
	}
	if got := do(t, m, "DELETE", "/nope").Code; got != 404 {
		t.Errorf("DELETE /nope = %d, want 404", got)
	}
}

func TestMuxTrailingSlash(t *testing.T) {
	m := NewMux()
	m.Get("/t/", func(w http.ResponseWriter, _ *http.Request, _ Params) { fmt.Fprint(w, "t") })
	m.Get("/u", func(w http.ResponseWriter, _ *http.Request, _ Params) { fmt.Fprint(w, "u") })

	if got := do(t, m, "GET", "/t/").Body.String(); got != "t" {
		t.Errorf("GET /t/ = %q, want t", got)
	}
	if got := do(t, m, "GET", "/t").Code; got != 404 {
		t.Errorf("GET /t = %d, want 404 (strict)", got)
	}
	if got := do(t, m, "GET", "/u").Body.String(); got != "u" {
		t.Errorf("GET /u = %q", got)
	}
	if got := do(t, m, "GET", "/u/").Code; got != 404 {
		t.Errorf("GET /u/ = %d, want 404 (strict)", got)
	}
}

func TestMuxTypedNotFoundAndMethodNotAllowed(t *testing.T) {
	m := NewMux()
	m.Get("/x", typed)
	// Default: empty 404 body, zero allocations.
	w := do(t, m, "GET", "/nope")
	if w.Code != 404 || w.Body.String() != "" {
		t.Errorf("default 404 = %d %q, want empty body", w.Code, w.Body.String())
	}
	// Custom handlers.
	m.NotFound(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(418) })
	m.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(419) })
	if got := do(t, m, "GET", "/nope").Code; got != 418 {
		t.Errorf("custom 404 = %d", got)
	}
	if got := do(t, m, "DELETE", "/x").Code; got != 419 {
		t.Errorf("custom 405 = %d", got)
	}
}

// TestMuxParams: names, order, At/Len, and the 8-param cap.
func TestMuxParams(t *testing.T) {
	m := NewMux()
	m.Get("/a/{x}/{y}/{z}", func(w http.ResponseWriter, _ *http.Request, ps Params) {
		if ps.Len() != 3 {
			t.Errorf("Len = %d", ps.Len())
		}
		n0, v0 := ps.At(0)
		n2, v2 := ps.At(2)
		fmt.Fprintf(w, "%s=%s,%s=%s,%s", n0, v0, n2, v2, ps.Get("y"))
	})
	if got := do(t, m, "GET", "/a/1/2/3").Body.String(); got != "x=1,z=3,2" {
		t.Errorf("params = %q", got)
	}

	// Nine params: the 9th is dropped (documented cap of 8).
	m2 := NewMux()
	m2.Get("/p/{a}/{b}/{c}/{d}/{e}/{f}/{g}/{h}/{i}", func(w http.ResponseWriter, _ *http.Request, ps Params) {
		fmt.Fprintf(w, "%d:%s", ps.Len(), ps.Get("i"))
	})
	if got := do(t, m2, "GET", "/p/1/2/3/4/5/6/7/8/9").Body.String(); got != "8:" {
		t.Errorf("cap = %q, want 8: (9th dropped)", got)
	}
}

// TestMuxCompatibleAdapter: GetFunc/HandleFunc publish params for URLParam and
// req.PathValue, and set up the routing Context.
func TestMuxCompatibleAdapter(t *testing.T) {
	m := NewMux()
	m.GetFunc("/c/{id}", func(w http.ResponseWriter, r *http.Request) {
		rctx := RouteContext(r.Context())
		if rctx == nil {
			t.Error("adapter should set the routing Context")
		}
		fmt.Fprintf(w, "%s/%s/%s", URLParam(r, "id"), r.PathValue("id"), Param(r, "id"))
	})
	if got := do(t, m, "GET", "/c/9").Body.String(); got != "9/9/9" {
		t.Errorf("adapter = %q, want 9/9/9", got)
	}
}

func TestMuxRoot(t *testing.T) {
	m := NewMux()
	m.Get("/", func(w http.ResponseWriter, _ *http.Request, _ Params) { fmt.Fprint(w, "root") })
	if got := do(t, m, "GET", "/").Body.String(); got != "root" {
		t.Errorf("root = %q", got)
	}
	if got := do(t, m, "GET", "/x").Code; got != 404 {
		t.Errorf("root is exact, /x = %d", got)
	}
}

// TestMuxConstraintBacktracking: two param edges at the same position, one
// constrained; the constrained one wins when it matches, the other otherwise.
func TestMuxConstraintBacktracking(t *testing.T) {
	m := NewMux()
	m.Get("/x/{id:[0-9]+}", func(w http.ResponseWriter, _ *http.Request, ps Params) {
		fmt.Fprint(w, "num:"+ps.Get("id"))
	})
	m.Get("/x/{name:[a-z]+}", func(w http.ResponseWriter, _ *http.Request, ps Params) {
		fmt.Fprint(w, "name:"+ps.Get("name"))
	})
	if got := do(t, m, "GET", "/x/123").Body.String(); got != "num:123" {
		t.Errorf("digits = %q", got)
	}
	if got := do(t, m, "GET", "/x/abc").Body.String(); got != "name:abc" {
		t.Errorf("letters = %q", got)
	}
	if got := do(t, m, "GET", "/x/1a").Code; got != 404 {
		t.Errorf("mixed = %d, want 404", got)
	}
}

func TestMuxTypedPanics(t *testing.T) {
	assertPanics(t, "no leading slash", func() { NewMux().Get("x", typed) })
	assertPanics(t, "missing brace", func() { NewMux().Get("/a/{id", typed) })
	assertPanics(t, "invalid constraint", func() { NewMux().Get("/a/{id:[}", typed) })
}

// TestParamsUnit tests the Params value type directly.
func TestParamsUnit(t *testing.T) {
	var keybuf [8]string
	var p Params
	p.keys = &keybuf
	p.Add("a", "1")
	p.Add("b", "2")
	if p.Len() != 2 || p.Get("a") != "1" || p.Get("b") != "2" {
		t.Fatalf("params = %d %q %q", p.Len(), p.Get("a"), p.Get("b"))
	}
	if p.Get("missing") != "" {
		t.Errorf("missing = %q", p.Get("missing"))
	}
	// Duplicate key: Get returns the newest.
	p.Add("a", "3")
	if got := p.Get("a"); got != "3" {
		t.Errorf("newest wins: %q", got)
	}
	// Cap at 8.
	for range 20 {
		p.Add("k", "v")
	}
	if p.Len() != 8 {
		t.Errorf("cap = %d, want 8", p.Len())
	}
	// Zero value is safe.
	var z Params
	if z.Len() != 0 || z.Get("x") != "" || z.ByName("x") != "" {
		t.Errorf("zero value not safe")
	}
	if n, v := z.At(0); n != "" || v != "" {
		t.Errorf("zero At = %q %q", n, v)
	}
}

func TestParamAccessorsOutsideRoute(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil).WithContext(t.Context())
	if got := Param(req, "id"); got != "" {
		t.Errorf("Param outside route = %q", got)
	}
	if got := ParamsOf(req); got.Len() != 0 {
		t.Errorf("ParamsOf outside route = %d", got.Len())
	}
}

// TestMuxDeepAndWildcardLongPath exercises longer paths and the SIMD/byte
// slash scan boundary.
func TestMuxDeepAndWildcardLongPath(t *testing.T) {
	m := NewMux()
	m.Get("/users/{id}/posts/{pid}/comments/{cid}", func(w http.ResponseWriter, _ *http.Request, ps Params) {
		fmt.Fprintf(w, "%s-%s-%s", ps.Get("id"), ps.Get("pid"), ps.Get("cid"))
	})
	m.Get("/s/*", func(w http.ResponseWriter, _ *http.Request, ps Params) { fmt.Fprint(w, ps.Get("*")) })

	if got := do(t, m, "GET", "/users/1/posts/2/comments/3").Body.String(); got != "1-2-3" {
		t.Errorf("deep = %q", got)
	}
	long := strings.Repeat("seg/", 40) + "end"
	if got := do(t, m, "GET", "/s/"+long).Body.String(); got != long {
		t.Errorf("long wildcard mismatch (len %d)", len(got))
	}
}

// TestMuxCompatAgree: on clean paths (no trailing slash, no "//", no escaping)
// where the two engines' semantics align, they must match the same routes with
// the same params.
func TestMuxCompatAgree(t *testing.T) {
	patterns := []string{"/ping", "/users/{id}", "/users/{id}/posts/{pid}", "/a/b/c"}

	compat := NewCompat()
	mux := NewMux()
	for _, p := range patterns {
		compat.Get(p, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, r.Pattern)
		})
		mux.Get(p, func(w http.ResponseWriter, r *http.Request, _ Params) {
			fmt.Fprint(w, r.Pattern)
		})
	}

	paths := []string{"/ping", "/users/42", "/users/42/posts/7", "/a/b/c", "/nope", "/users/42/x", "/a/b"}
	for _, p := range paths {
		cw := do(t, compat, "GET", p)
		mw := do(t, mux, "GET", p)
		if cw.Code != mw.Code {
			t.Errorf("%s: status compat=%d mux=%d", p, cw.Code, mw.Code)
		}
		// The 404 body differs by design (Compat keeps net/http's, Mux is
		// empty), so only compare the matched pattern.
		if cw.Code < 300 && cw.Body.String() != mw.Body.String() {
			t.Errorf("%s: pattern compat=%q mux=%q", p, cw.Body.String(), mw.Body.String())
		}
	}
}

// FuzzMux: arbitrary method/path must never panic and must answer with a valid
// status (204 from the handlers, or 404/405).
func FuzzMux(f *testing.F) {
	m := NewMux()
	typed := func(w http.ResponseWriter, _ *http.Request, _ Params) { w.WriteHeader(204) }
	m.Get("/ping", typed)
	m.Get("/users/{id}", typed)
	m.Get("/users/{id:[0-9]+}/x", typed)
	m.Get("/d/{m:[0-9]+}-{d:[0-9]+}", typed)
	m.Get("/files/*", typed)
	m.Get("/t/", typed)
	m.Get("/", typed)
	m.Handle("PURGE", "/p", typed)

	f.Add("/users/42", "GET")
	f.Add("/files/a/b", "GET")
	f.Add("/t/", "GET")
	f.Add("/", "POST")
	f.Add("//", "GET")
	f.Add("/d/1-2", "GET")
	f.Add("/p", "PURGE")
	f.Add("/users/abc/x", "GET")

	f.Fuzz(func(t *testing.T, path, method string) {
		if method == "" {
			method = "GET"
		}
		req := &http.Request{Method: method, URL: &url.URL{Path: path}}
		w := httptest.NewRecorder()
		m.ServeHTTP(w, req)
		switch w.Code {
		case 204, 404, 405:
		default:
			t.Fatalf("%s %q -> %d", method, path, w.Code)
		}
	})
}

// TestMuxSameShapeDifferentNames: the same shape with different param names,
// on different methods, must expose the right name per method (like Chi's
// per-endpoint param keys).
func TestMuxSameShapeDifferentNames(t *testing.T) {
	m := NewMux()
	m.Get("/u/{id}", func(w http.ResponseWriter, _ *http.Request, ps Params) {
		fmt.Fprint(w, "get:"+ps.Get("id"))
	})
	m.Post("/u/{name}", func(w http.ResponseWriter, _ *http.Request, ps Params) {
		fmt.Fprint(w, "post:"+ps.Get("name"))
	})
	if got := do(t, m, "GET", "/u/1").Body.String(); got != "get:1" {
		t.Errorf("GET = %q, want get:1", got)
	}
	if got := do(t, m, "POST", "/u/2").Body.String(); got != "post:2" {
		t.Errorf("POST = %q, want post:2", got)
	}
}

func TestMuxDuplicateRouteLastWins(t *testing.T) {
	m := NewMux()
	m.Get("/x", func(w http.ResponseWriter, _ *http.Request, _ Params) { fmt.Fprint(w, "first") })
	m.Get("/x", func(w http.ResponseWriter, _ *http.Request, _ Params) { fmt.Fprint(w, "second") })
	if got := do(t, m, "GET", "/x").Body.String(); got != "second" {
		t.Errorf("duplicate = %q, want second", got)
	}
}

func TestMuxWildcardAfterParam(t *testing.T) {
	m := NewMux()
	m.Get("/a/{x}/*", func(w http.ResponseWriter, _ *http.Request, ps Params) {
		fmt.Fprintf(w, "%s|%s", ps.Get("x"), ps.Get("*"))
	})
	if got := do(t, m, "GET", "/a/1/b/c").Body.String(); got != "1|b/c" {
		t.Errorf("wild after param = %q", got)
	}
	if got := do(t, m, "GET", "/a/1/").Body.String(); got != "1|" {
		t.Errorf("wild empty = %q", got)
	}
	if got := do(t, m, "GET", "/a/1").Code; got != 404 {
		t.Errorf("/a/1 = %d, want 404", got)
	}
}

func TestMuxAllowWithCustomMethod(t *testing.T) {
	m := NewMux()
	m.Get("/x", typed)
	m.Handle("PURGE", "/x", typed)
	w := do(t, m, "DELETE", "/x")
	if w.Code != 405 {
		t.Fatalf("code = %d", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD, PURGE" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD, PURGE")
	}
}

func TestMuxAtBounds(t *testing.T) {
	m := NewMux()
	m.Get("/x/{a}", func(w http.ResponseWriter, _ *http.Request, ps Params) {
		n, v := ps.At(-1)
		n2, v2 := ps.At(99)
		fmt.Fprintf(w, "%q%q%q%q", n, v, n2, v2)
	})
	if got := do(t, m, "GET", "/x/1").Body.String(); got != `""""""""` {
		t.Errorf("At bounds = %q", got)
	}
}

func TestMuxHandleFuncMethod(t *testing.T) {
	m := NewMux()
	m.HandleFunc("PUT", "/h/{id}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, URLParam(r, "id"))
	})
	if got := do(t, m, "PUT", "/h/7").Body.String(); got != "7" {
		t.Errorf("HandleFunc = %q", got)
	}
	if got := do(t, m, "GET", "/h/7").Code; got != 405 {
		t.Errorf("GET on PUT route = %d, want 405", got)
	}
}

func mwTag(tag string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("X-MW", tag)
			next.ServeHTTP(w, r)
		})
	}
}

func TestMuxUse(t *testing.T) {
	inits := 0
	mw := func(next http.Handler) http.Handler {
		inits++
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Root", "1")
			next.ServeHTTP(w, r)
		})
	}
	m := NewMux()
	m.Use(mw)
	m.Get("/a/{id}", func(w http.ResponseWriter, _ *http.Request, ps Params) { fmt.Fprint(w, ps.Get("id")) })

	w := do(t, m, "GET", "/a/7")
	if w.Body.String() != "7" || w.Header().Get("X-Root") != "1" {
		t.Fatalf("Use: body=%q root=%q", w.Body.String(), w.Header().Get("X-Root"))
	}
	if inits != 1 {
		t.Errorf("middleware constructor ran %d times, want 1", inits)
	}
	// A middleware sees params after next, like Chi.
	m2 := NewMux()
	m2.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			w.Header().Set("X-Param", Param(r, "id"))
		})
	})
	m2.Get("/b/{id}", typed)
	if got := do(t, m2, "GET", "/b/9").Header().Get("X-Param"); got != "9" {
		t.Errorf("param in middleware after next = %q", got)
	}
}

func TestMuxWithGroup(t *testing.T) {
	m := NewMux()
	m.Get("/plain", typed)
	m.With(mwTag("with")).Get("/w", typed)
	m.Group(func(g *Mux) {
		g.Use(mwTag("group"))
		g.Get("/g", typed)
	})

	if got := do(t, m, "GET", "/plain").Header().Get("X-MW"); got != "" {
		t.Errorf("plain leaked middleware: %q", got)
	}
	if got := do(t, m, "GET", "/w").Header().Get("X-MW"); got != "with" {
		t.Errorf("With = %q", got)
	}
	if got := do(t, m, "GET", "/g").Header().Get("X-MW"); got != "group" {
		t.Errorf("Group = %q", got)
	}
}

func TestMuxRoutePrefix(t *testing.T) {
	m := NewMux()
	m.Route("/api/{v}", func(r *Mux) {
		r.Get("/users/{id}", func(w http.ResponseWriter, req *http.Request, ps Params) {
			fmt.Fprintf(w, "%s/%s/%s", ps.Get("v"), ps.Get("id"), req.Pattern)
		})
		r.Route("/nested", func(r *Mux) {
			r.Get("/x", func(w http.ResponseWriter, _ *http.Request, ps Params) { fmt.Fprint(w, ps.Get("v")) })
		})
	})
	if got := do(t, m, "GET", "/api/v1/users/9").Body.String(); got != "v1/9//api/{v}/users/{id}" {
		t.Errorf("Route = %q", got)
	}
	if got := do(t, m, "GET", "/api/v2/nested/x").Body.String(); got != "v2" {
		t.Errorf("nested Route = %q", got)
	}
	if got := do(t, m, "GET", "/api/v1/nope").Code; got != 404 {
		t.Errorf("miss = %d", got)
	}
}

func TestMuxMount(t *testing.T) {
	sub := NewMux()
	sub.Get("/inner/{x}", func(w http.ResponseWriter, _ *http.Request, ps Params) { fmt.Fprint(w, ps.Get("x")) })

	m := NewMux()
	m.Route("/api/{v}", func(r *Mux) { r.Mount("/sub", sub) })

	if got := do(t, m, "GET", "/api/v1/sub/inner/9").Body.String(); got != "9" {
		t.Errorf("mounted Mux = %q", got)
	}
	// Bare mount path: the sub sees "/".
	sub2 := NewMux()
	sub2.Get("/", func(w http.ResponseWriter, _ *http.Request, _ Params) { fmt.Fprint(w, "root") })
	m2 := NewMux()
	m2.Mount("/h", sub2)
	if got := do(t, m2, "GET", "/h").Body.String(); got != "root" {
		t.Errorf("bare mount = %q", got)
	}
	// Mounting a plain http.Handler: the path is stripped.
	m3 := NewMux()
	m3.Mount("/files", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, req.URL.Path)
	}))
	if got := do(t, m3, "GET", "/files/a/b").Body.String(); got != "/a/b" {
		t.Errorf("mount handler path = %q", got)
	}
	if got := do(t, m3, "GET", "/files").Body.String(); got != "/" {
		t.Errorf("mount bare path = %q", got)
	}
}

func TestMuxMethodBeatsMount(t *testing.T) {
	sub := NewMux()
	sub.HandleAll("/", func(w http.ResponseWriter, _ *http.Request, _ Params) { fmt.Fprint(w, "sub") })
	m := NewMux()
	m.Mount("/x", sub)
	m.Get("/x", func(w http.ResponseWriter, _ *http.Request, _ Params) { fmt.Fprint(w, "get") })
	if got := do(t, m, "GET", "/x").Body.String(); got != "get" {
		t.Errorf("method route should beat the mount: %q", got)
	}
	if got := do(t, m, "POST", "/x").Body.String(); got != "sub" {
		t.Errorf("other method falls to the mount: %q", got)
	}
}
