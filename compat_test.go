package router

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ok(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

func body(s string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, s) }
}

func assertPanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: expected panic", name)
		}
	}()
	fn()
}

// TestMuxVerbs covers every method helper plus Method/MethodFunc and the
// Handle/HandleFunc "METHOD /path" form.
func TestMuxVerbs(t *testing.T) {
	r := NewRouter()
	r.Connect("/c", ok)
	r.Delete("/d", ok)
	r.Get("/g", ok)
	r.Head("/h", ok)
	r.Options("/o", ok)
	r.Patch("/p", ok)
	r.Post("/po", ok)
	r.Put("/pu", ok)
	r.Query("/q", ok)
	r.Trace("/tr", ok)
	r.Method("PURGE", "/m", http.HandlerFunc(ok))
	r.MethodFunc("LOCK", "/mf", ok)
	r.Handle("/any", http.HandlerFunc(ok))
	r.Handle("REPORT /rep", http.HandlerFunc(ok))
	r.HandleFunc("COPY /cp", ok)
	r.HandleFunc("/hf", ok)

	cases := []struct{ method, path string }{
		{"CONNECT", "/c"}, {"DELETE", "/d"}, {"GET", "/g"}, {"HEAD", "/h"},
		{"OPTIONS", "/o"}, {"PATCH", "/p"}, {"POST", "/po"}, {"PUT", "/pu"},
		{"QUERY", "/q"}, {"TRACE", "/tr"}, {"PURGE", "/m"}, {"LOCK", "/mf"},
		{"GET", "/any"}, {"POST", "/any"}, {"REPORT", "/rep"}, {"COPY", "/cp"},
		{"PATCH", "/hf"},
	}
	for _, c := range cases {
		if got := do(t, r, c.method, c.path).Code; got != http.StatusNoContent {
			t.Errorf("%s %s = %d, want 204", c.method, c.path, got)
		}
	}
}

func TestMuxNotFoundAndMethodNotAllowed(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id}", body("u"))
	r.Post("/users/{id}", body("c"))

	// Default 404.
	if got := do(t, r, "GET", "/nope"); got.Code != http.StatusNotFound || got.Body.String() != "404 page not found\n" {
		t.Errorf("default 404 = %d %q", got.Code, got.Body.String())
	}
	// Default 405 with Allow header (GET implies HEAD).
	w := do(t, r, "DELETE", "/users/1")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("default 405 = %d", w.Code)
	}
	allow := strings.Join(w.Header().Values("Allow"), ",")
	if !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") || !strings.Contains(allow, "HEAD") {
		t.Errorf("Allow = %q", allow)
	}
	// A route matching the path under no method (validator rejects) is a 404.
	if got := do(t, r, "PUT", "/users/1"); got.Code != http.StatusMethodNotAllowed {
		t.Errorf("405 for other method = %d", got.Code)
	}

	// Custom handlers.
	r2 := NewRouter()
	r2.Get("/x", body("x"))
	r2.NotFound(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(418) })
	r2.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(491) })
	if got := do(t, r2, "GET", "/missing"); got.Code != 418 {
		t.Errorf("custom 404 = %d", got.Code)
	}
	if got := do(t, r2, "POST", "/x"); got.Code != 491 {
		t.Errorf("custom 405 = %d", got.Code)
	}
}

// TestMuxMixedSegment exercises the {a}-{b} desugaring end to end, including
// the validator-driven 404/405 paths that matchShape/checkMixed serve.
func TestMuxMixedSegment(t *testing.T) {
	r := NewRouter()
	r.Get("/d/{month:[0-9]{2}}-{day:[0-9]{2}}", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "%s/%s", URLParam(req, "month"), URLParam(req, "day"))
	})

	if got := do(t, r, "GET", "/d/09-21").Body.String(); got != "09/21" {
		t.Errorf("mixed = %q", got)
	}
	if got := do(t, r, "GET", "/d/9-21").Code; got != http.StatusNotFound {
		t.Errorf("month too short = %d, want 404", got)
	}
	if got := do(t, r, "GET", "/d/09-2x").Code; got != http.StatusNotFound {
		t.Errorf("non-digit day = %d, want 404", got)
	}
	// Wrong method on an otherwise matching mixed route: 405 (not 404).
	if got := do(t, r, "POST", "/d/09-21").Code; got != http.StatusMethodNotAllowed {
		t.Errorf("mixed wrong method = %d, want 405", got)
	}
	// Find must return the pattern for the matching path.
	if got := r.Find(NewRouteContext(), "GET", "/d/09-21"); got != "/d/{month:[0-9]{2}}-{day:[0-9]{2}}" {
		t.Errorf("Find = %q", got)
	}
}

func TestMuxTrailingSlashRootAndWildcard(t *testing.T) {
	r := NewRouter()
	r.Get("/", body("root"))
	r.Route("/articles", func(r Router) {
		r.Get("/", body("list"))
	})
	r.Get("/files/*", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "rest=%s|star=%s", URLParam(req, "rest"), URLParam(req, "*"))
	})

	for _, p := range []string{"/articles", "/articles/"} {
		if got := do(t, r, "GET", p).Body.String(); got != "list" {
			t.Errorf("GET %s = %q", p, got)
		}
	}
	if got := do(t, r, "GET", "/").Body.String(); got != "root" {
		t.Errorf("root = %q", got)
	}
	// Root must not swallow everything (it is anchored to "/").
	if got := do(t, r, "GET", "/other").Code; got != http.StatusNotFound {
		t.Errorf("root catch-all leak = %d", got)
	}
	if got := do(t, r, "GET", "/files/a/b/c").Body.String(); got != "rest=a/b/c|star=a/b/c" {
		t.Errorf("wildcard = %q", got)
	}
}

func TestMuxUnsafeParamNames(t *testing.T) {
	r := NewRouter()
	// Names stdlib rejects (hyphen, "rest") must still work via placeholders.
	r.Get("/a/{foo-bar}", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, URLParam(req, "foo-bar"))
	})
	r.Get("/b/{rest}", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, URLParam(req, "rest"))
	})
	if got := do(t, r, "GET", "/a/x1").Body.String(); got != "x1" {
		t.Errorf("hyphen param = %q", got)
	}
	if got := do(t, r, "GET", "/b/y1").Body.String(); got != "y1" {
		t.Errorf("rest param = %q", got)
	}
}

func TestMuxMultipleParamsAndConstraints(t *testing.T) {
	r := NewRouter()
	r.Get("/u/{id:[0-9]+}/p/{page:[a-z]{2,4}}", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "%s-%s", URLParam(req, "id"), URLParam(req, "page"))
	})
	if got := do(t, r, "GET", "/u/42/p/ab").Body.String(); got != "42-ab" {
		t.Errorf("multi param = %q", got)
	}
	if got := do(t, r, "GET", "/u/4x/p/ab").Code; got != http.StatusNotFound {
		t.Errorf("bad id = %d", got)
	}
	if got := do(t, r, "GET", "/u/42/p/a").Code; got != http.StatusNotFound {
		t.Errorf("short page = %d", got)
	}
}

func TestMuxMountNestedAndParentParams(t *testing.T) {
	sub := NewRouter()
	sub.Get("/inner/{x}", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, URLParam(req, "x"))
	})
	v1 := NewRouter()
	v1.Mount("/api", sub)

	r := NewRouter()
	r.Route("/v1", func(r Router) { r.Mount("/api", sub) })
	_ = v1

	if got := do(t, r, "GET", "/v1/api/inner/9").Body.String(); got != "9" {
		t.Errorf("nested mount = %q", got)
	}
	if got := do(t, r, "GET", "/v1/api/inner").Code; got != http.StatusNotFound {
		t.Errorf("missing param = %d", got)
	}

	// Mount a plain http.Handler (not a Router).
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "pong") })
	r2 := NewRouter()
	r2.Mount("/h", mux)
	if got := do(t, r2, "GET", "/h/ping").Body.String(); got != "pong" {
		t.Errorf("mount handler = %q", got)
	}
}

func TestMuxFindAndMatch(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id}", ok)
	r.Post("/users/{id}", ok)
	r.Get("/files/*", ok)
	sub := NewRouter()
	sub.Get("/leaf", ok)
	r.Mount("/api", sub)

	cases := []struct{ method, path, want string }{
		{"GET", "/users/7", "/users/{id}"},
		{"POST", "/users/7", "/users/{id}"},
		{"HEAD", "/users/7", "/users/{id}"}, // HEAD matches GET like stdlib
		{"GET", "/files/a/b", "/files/*"},
		{"GET", "/api/leaf", "/api/leaf"},
		{"GET", "/nope", ""},
		{"GET", "/users/7/extra", ""},
	}
	for _, c := range cases {
		if got := r.Find(NewRouteContext(), c.method, c.path); got != c.want {
			t.Errorf("Find(%s %s) = %q, want %q", c.method, c.path, got, c.want)
		}
		if want := c.want != ""; r.Match(NewRouteContext(), c.method, c.path) != want {
			t.Errorf("Match(%s %s) = %v, want %v", c.method, c.path, !want, want)
		}
	}
	// Find populates the routing context params.
	rctx := NewRouteContext()
	r.Find(rctx, "GET", "/users/7")
	if got := rctx.URLParam("id"); got != "7" {
		t.Errorf("Find params = %q", got)
	}
}

func TestMuxRoutes(t *testing.T) {
	r := NewRouter()
	r.Get("/users/{id}", ok)
	r.Post("/users/{id}", ok)
	sub := NewRouter()
	sub.Get("/leaf", ok)
	r.Mount("/api", sub)

	var hasUsers, hasMount bool
	for _, rt := range r.Routes() {
		switch rt.Pattern {
		case "/users/{id}":
			hasUsers = true
			if len(rt.Handlers) != 2 || rt.Handlers["GET"] == nil || rt.Handlers["POST"] == nil {
				t.Errorf("users handlers = %v", rt.Handlers)
			}
		case "/api":
			hasMount = true
			if rt.SubRoutes == nil {
				t.Errorf("mount missing SubRoutes")
			}
		}
	}
	if !hasUsers || !hasMount {
		t.Errorf("Routes missing entries: users=%v mount=%v", hasUsers, hasMount)
	}
}

func TestMuxWalk(t *testing.T) {
	var mw = func(s string) Middleware {
		return func(next http.Handler) http.Handler { return next }
	}
	r := NewRouter()
	r.Use(mw("root"))
	_ = r.Get
	r.Get("/users/{id}", ok)
	r.Route("/api", func(r Router) {
		r.Use(mw("sub"))
		r.Get("/leaf", ok)
	})

	got := map[string]int{} // "METHOD route" -> middleware count
	err := Walk(r, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		got[method+" "+route] = len(mws)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if _, ok := got["GET /users/{id}"]; !ok {
		t.Errorf("Walk missing users route: %v", got)
	}
	if n, ok := got["GET /api/leaf"]; !ok || n < 2 {
		t.Errorf("Walk api/leaf = %d mws, want >=2: %v", n, got)
	}

	// Walk stops on the first error.
	sentinel := errors.New("stop")
	if err := Walk(r, func(string, string, http.Handler, ...func(http.Handler) http.Handler) error {
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Errorf("Walk error = %v", err)
	}
}

func TestMuxMiddlewares(t *testing.T) {
	mw := func(http.Handler) http.Handler { return nil }
	r := NewRouter()
	r.Use(mw, mw)
	if got := len(r.Middlewares()); got != 2 {
		t.Errorf("root Middlewares = %d", got)
	}
	sub := r.With(mw)
	if got := len(sub.Middlewares()); got != 1 {
		t.Errorf("With Middlewares = %d", got)
	}
}

func TestMuxRoutePatternInContext(t *testing.T) {
	// Plain param routes carry no routing Context (fast path): the matched
	// pattern is on req.Pattern, set from the stdlib mux. RoutePattern() is
	// available once the router uses features that need the Context (mounts,
	// wildcards, mixed/renamed params).
	r := NewRouter()
	r.Get("/users/{id}", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, req.Pattern)
	})
	if got := do(t, r, "GET", "/users/9").Body.String(); got != "/users/{id}" {
		t.Errorf("req.Pattern = %q", got)
	}

	r2 := NewRouter()
	sub := NewRouter()
	sub.Get("/leaf/{x}", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, RouteContext(req.Context()).RoutePattern())
	})
	r2.Mount("/api", sub)
	if got := do(t, r2, "GET", "/api/leaf/1").Body.String(); got != "/api/leaf/{x}" {
		t.Errorf("mounted RoutePattern = %q", got)
	}
}

func TestMuxRegisterMethod(t *testing.T) {
	RegisterMethod("")       // no-op
	RegisterMethod("purge")  // records (case-insensitive), no panic
	RegisterMethod("PURGE")  // idempotent
	RegisterMethod("PURGE ") // distinct entry, still fine
}

func TestContextHelpers(t *testing.T) {
	var k contextKey
	if got := k.String(); !strings.Contains(got, "router context value") {
		t.Errorf("contextKey.String = %q", got)
	}
	// URLParam on a request with no routing context falls back to PathValue.
	req := httptest.NewRequest("GET", "/x", nil).WithContext(t.Context())
	req.SetPathValue("v", "42")
	if got := URLParam(req, "v"); got != "42" {
		t.Errorf("URLParam fallback = %q", got)
	}
	if got := URLParamFromCtx(req.Context(), "v"); got != "" {
		t.Errorf("URLParamFromCtx without rctx = %q", got)
	}
}

func TestMuxFileServer(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewRouter()
	FileServer(r, "/static", http.Dir(dir))
	if got := do(t, r, "GET", "/static/a.txt").Body.String(); got != "hello" {
		t.Errorf("FileServer body = %q", got)
	}
	if got := do(t, r, "GET", "/static").Code; got != http.StatusMovedPermanently {
		t.Errorf("FileServer redirect = %d", got)
	}
	assertPanics(t, "FileServer params", func() { FileServer(NewRouter(), "/s/{x}", http.Dir(dir)) })
}

func TestMuxPanics(t *testing.T) {
	assertPanics(t, "Use after route", func() {
		r := NewRouter()
		r.Get("/x", ok)
		r.Use(func(next http.Handler) http.Handler { return next })
	})
	assertPanics(t, "Route nil fn", func() {
		NewRouter().Route("/x", nil)
	})
	assertPanics(t, "Mount nil", func() {
		NewRouter().Mount("/x", nil)
	})
	assertPanics(t, "Mount duplicate", func() {
		r := NewRouter()
		r.Mount("/x", NewRouter())
		r.Mount("/x", NewRouter())
	})
	assertPanics(t, "Mount self", func() {
		r := NewRouter()
		r.Mount("/x", r)
	})
	assertPanics(t, "pattern no slash", func() {
		NewRouter().Get("x", ok)
	})
	assertPanics(t, "duplicate param", func() {
		NewRouter().Get("/{id}/{id}", ok)
	})
	assertPanics(t, "wildcard not last", func() {
		NewRouter().Get("/a/*/b", ok)
	})
	assertPanics(t, "missing brace", func() {
		NewRouter().Get("/a/{id", ok)
	})
	assertPanics(t, "invalid constraint", func() {
		NewRouter().Get(`/a/{id:[}`, ok)
	})
	assertPanics(t, "unsupported constraint", func() {
		NewRouter().Get(`/a/{id:\p{L}+}`, ok)
	})
}

// TestMuxChiExamples ports the routing shapes from Chi's README.
func TestMuxChiExamples(t *testing.T) {
	r := NewRouter()
	r.Get("/", body("hi"))
	r.Route("/articles", func(r Router) {
		r.Get("/", body("list"))
		r.Get("/{month}-{day}-{year}", func(w http.ResponseWriter, req *http.Request) {
			fmt.Fprintf(w, "%s/%s/%s", URLParam(req, "month"), URLParam(req, "day"), URLParam(req, "year"))
		})
		r.Post("/", body("create"))
		r.Get("/search", body("search"))
		r.Get("/{articleSlug:[a-z-]+}", body("slug"))
		r.Route("/{articleID}", func(r Router) {
			r.Get("/", body("article"))
			r.Put("/", body("update"))
			r.Delete("/", body("delete"))
		})
	})
	admin := NewRouter()
	admin.Get("/accounts", body("accounts"))
	r.Mount("/admin", admin)

	cases := []struct{ method, path, want string }{
		{"GET", "/", "hi"},
		{"GET", "/articles", "list"},
		{"POST", "/articles", "create"},
		{"GET", "/articles/search", "search"},
		{"GET", "/articles/home-is-toronto", "slug"},
		{"GET", "/articles/01-16-2017", "01/16/2017"},
		{"GET", "/articles/123", "article"},
		{"PUT", "/articles/123", "update"},
		{"DELETE", "/articles/123", "delete"},
		{"GET", "/admin/accounts", "accounts"},
	}
	for _, c := range cases {
		if got := do(t, r, c.method, c.path).Body.String(); got != c.want {
			t.Errorf("%s %s = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

func do(t *testing.T, r http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil).WithContext(t.Context())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestCore(t *testing.T) {
	r := NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-MW", "1")
			next.ServeHTTP(w, req)
		})
	})
	r.Get("/users/{id}", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, "user="+URLParam(req, "id"))
	})
	r.Post("/users", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "created") })
	r.Route("/articles", func(r Router) {
		r.Get("/{id}", func(w http.ResponseWriter, req *http.Request) {
			fmt.Fprint(w, "art="+URLParam(req, "id"))
		})
	})
	r.Group(func(r Router) {
		r.Get("/grouped", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "g") })
	})
	r.With(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-With", "yes")
			next.ServeHTTP(w, req)
		})
	}).Get("/special", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "s") })
	r.Get("/files/*", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, "file="+URLParam(req, "rest"))
	})
	r.Get("/re/{id:[0-9]+}", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, "re="+URLParam(req, "id"))
	})
	sub := NewRouter()
	sub.Get("/inner/{x}", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, "mount="+URLParam(req, "x"))
	})
	r.Mount("/api", sub)
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		fmt.Fprint(w, "nf")
	})

	cases := []struct{ method, path, want string }{
		{"GET", "/users/42", "user=42"},
		{"POST", "/users", "created"},
		{"GET", "/articles/7", "art=7"},
		{"GET", "/grouped", "g"},
		{"GET", "/special", "s"},
		{"GET", "/files/a/b/c", "file=a/b/c"},
		{"GET", "/re/123", "re=123"},
		{"GET", "/api/inner/9", "mount=9"},
		{"GET", "/nope", "nf"},
	}
	for _, c := range cases {
		if got := do(t, r, c.method, c.path).Body.String(); got != c.want {
			t.Errorf("%s %s = %q, want %q", c.method, c.path, got, c.want)
		}
	}
	if got := do(t, r, "GET", "/users/1").Header().Get("X-MW"); got != "1" {
		t.Errorf("global middleware not applied")
	}
	if got := do(t, r, "GET", "/special").Header().Get("X-With"); got != "yes" {
		t.Errorf("With middleware not applied")
	}
	if got := do(t, r, "GET", "/users/1").Header().Get("X-With"); got != "" {
		t.Errorf("With middleware leaked to other routes")
	}
}

// TestAPIErgonomics exercises the redesigned surface: Router interface in
// closures, Param/ParamsOf accessors, Params.Get/At/Len, and Any.
func TestAPIErgonomics(t *testing.T) {
	r := NewCompat()
	r.Use(func(next http.Handler) http.Handler { return next })

	// Closures take the Router interface, like chi.Router.
	r.Route("/api", func(r Router) {
		r.Get("/users/{id}", func(w http.ResponseWriter, req *http.Request) {
			fmt.Fprintf(w, "id=%s path=%s", Param(req, "id"), req.PathValue("id"))
		})
		r.Get("/orders/{id}/{item}", func(w http.ResponseWriter, req *http.Request) {
			ps := ParamsOf(req)
			var names []string
			for i := 0; i < ps.Len(); i++ {
				n, _ := ps.At(i)
				names = append(names, n)
			}
			fmt.Fprintf(w, "%s:%s:%v", ps.Get("id"), ps.Get("item"), names)
		})
		r.Any("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	})

	if got := do(t, r, "GET", "/api/users/7").Body.String(); got != "id=7 path=7" {
		t.Errorf("Param/PathValue = %q", got)
	}
	if got := do(t, r, "GET", "/api/orders/9/x").Body.String(); got != "9:x:[* id item]" {
		t.Errorf("ParamsOf = %q", got)
	}
	for _, m := range []string{"GET", "POST", "DELETE"} {
		if got := do(t, r, m, "/api/health").Code; got != 204 {
			t.Errorf("Any %s = %d", m, got)
		}
	}
}

// TestCompatParamsAcrossNestedMounts: params accumulate across nested mounts.
func TestCompatParamsAcrossNestedMounts(t *testing.T) {
	r := NewRouter()
	r.Route("/a/{id}", func(r Router) {
		r.Route("/b/{sub}", func(r Router) {
			r.Get("/c", func(w http.ResponseWriter, req *http.Request) {
				fmt.Fprintf(w, "%s/%s", Param(req, "id"), Param(req, "sub"))
			})
		})
	})
	if got := do(t, r, "GET", "/a/1/b/2/c").Body.String(); got != "1/2" {
		t.Errorf("nested params = %q, want 1/2", got)
	}
}

// TestCompatParamShadowingNewestWins: a child param with the same name shadows
// the parent's, like Chi.
func TestCompatParamShadowingNewestWins(t *testing.T) {
	r := NewRouter()
	r.Route("/a/{id}", func(r Router) {
		r.Get("/b/{id}", func(w http.ResponseWriter, req *http.Request) {
			fmt.Fprint(w, Param(req, "id"))
		})
	})
	if got := do(t, r, "GET", "/a/1/b/2").Body.String(); got != "2" {
		t.Errorf("shadowing = %q, want 2 (newest)", got)
	}
}

// TestCompatMixedConstraintMiss: mixed + constraint routes still answer
// 404/405 correctly through the miss path.
func TestCompatMixedConstraintMiss(t *testing.T) {
	r := NewRouter()
	r.Get("/d/{m:[0-9]{2}}-{d:[0-9]{2}}", noopBody("ok"))
	r.Post("/d/{m:[0-9]{2}}-{d:[0-9]{2}}", noopBody("post"))

	if got := do(t, r, "GET", "/d/09-21").Body.String(); got != "ok" {
		t.Errorf("match = %q", got)
	}
	if got := do(t, r, "GET", "/d/9-21").Code; got != 404 {
		t.Errorf("bad constraint = %d, want 404", got)
	}
	if got := do(t, r, "DELETE", "/d/09-21").Code; got != 405 {
		t.Errorf("wrong method = %d, want 405", got)
	}
}

func TestCompatAnyAllMethods(t *testing.T) {
	r := NewRouter()
	r.Any("/any", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	for _, m := range []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		if got := do(t, r, m, "/any").Code; got != 204 {
			t.Errorf("Any %s = %d", m, got)
		}
	}
}

func noopBody(s string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, s) }
}
