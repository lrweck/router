// Package routerbench compares this router with the popular Go routers.
//
// Separate, optional module (not part of the library build):
//
//	cd bench && go test -run='^$' -bench=. -benchmem
//
// Every target is driven through the same discard writer, so the numbers
// isolate routing/dispatch. Routers with a fast native handler API are tested
// in that mode, and the http.Handler-compatible one as "<name>-http". Handlers
// write 204 and read the first path param where the API allows it.
//
// Fiber is fasthttp-based: it is driven through app.Handler() (a
// fasthttp.RequestHandler) with a reused RequestCtx; "<name>-http" uses the
// net/http adaptor. Its absolute numbers are not apples-to-apples with the
// net/http routers (different server model), but the routing cost is visible.
//
// The constrained-* scenarios use realistic constraints (digits, UUID, date,
// slug). Only the routers that support regex constraints (this package and
// chi, gorilla) actually validate them; the rest register a plain param, so
// their numbers there are the no-validation cost.
package routerbench

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	chi "github.com/go-chi/chi/v5"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"github.com/gorilla/mux"
	"github.com/julienschmidt/httprouter"
	"github.com/labstack/echo/v4"
	"github.com/lrweck/router"
	"github.com/uptrace/bunrouter"
	"github.com/valyala/fasthttp"
)

const (
	pStatic = "/ping"
	pParam  = "/users/42"
	pDeep   = "/users/42/posts/7/comments/9"
	pWild   = "/static/js/app.js"
	pMiss   = "/nope/nothing"
	pMany   = "/r49/42"
	pLong   = "/users/" + "0123456789" + "0123456789" + "0123456789" + "0123456789" +
		"0123456789" + "0123456789" + "0123456789" + "0123456789" + "0123456789" + "0123456789"

	// Realistic constrained params.
	pConstrained = "/c/42"
	pUUID        = "/u/550e8400-e29b-41d4-a716-446655440000"
	pBlog        = "/blog/2026-09-21/my-post-title"
	pSlug        = "/a/home-is-toronto"
)

const many = 50

// Regex sources (used only where the router supports them).
const (
	reDigits = "[0-9]+"
	reUUID   = "[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"
	reDate   = "[0-9]{4}-[0-9]{2}-[0-9]{2}"
	reSlug   = "[a-z0-9-]+"
)

// ---- runners ----

type runner interface {
	run(b *testing.B, method, path string, want int)
	runParallel(b *testing.B, method, path string, want int)
}

type discardRW struct {
	h    http.Header
	code int
}

func (d *discardRW) Header() http.Header {
	if d.h == nil {
		d.h = http.Header{}
	}
	return d.h
}
func (d *discardRW) Write(p []byte) (int, error) { return len(p), nil }
func (d *discardRW) WriteHeader(c int)           { d.code = c }

type httpRunner struct{ h http.Handler }

func (r httpRunner) run(b *testing.B, method, path string, want int) {
	req := httptest.NewRequest(method, path, nil)
	d := &discardRW{}
	b.ReportAllocs()
	for b.Loop() {
		d.code = 0
		r.h.ServeHTTP(d, req)
		if d.code != want {
			b.Fatalf("code=%d want=%d", d.code, want)
		}
	}
}

// runParallel drives the router from b.RunParallel. Each goroutine gets its
// own request and ResponseWriter: routers may set r.Pattern / path values on
// the request they are handed, so sharing one would race.

type fiberRunner struct{ h fasthttp.RequestHandler }

func (r fiberRunner) run(b *testing.B, method, path string, want int) {
	var ctx fasthttp.RequestCtx
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(path)
	b.ReportAllocs()
	for b.Loop() {
		ctx.Response.Reset()
		r.h(&ctx)
		if got := ctx.Response.StatusCode(); got != want {
			b.Fatalf("code=%d want=%d", got, want)
		}
	}
}

func (r httpRunner) runParallel(b *testing.B, method, path string, want int) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest(method, path, nil)
		d := &discardRW{}
		for pb.Next() {
			d.code = 0
			r.h.ServeHTTP(d, req)
			if d.code != want {
				b.Errorf("code=%d want=%d", d.code, want)
			}
		}
	})
}

func (r fiberRunner) runParallel(b *testing.B, method, path string, want int) {
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var ctx fasthttp.RequestCtx
		ctx.Request.Header.SetMethod(method)
		ctx.Request.SetRequestURI(path)
		for pb.Next() {
			ctx.Response.Reset()
			r.h(&ctx)
			if ctx.Response.StatusCode() != want {
				b.Errorf("code=%d want=%d", ctx.Response.StatusCode(), want)
			}
		}
	})
}

func noop(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

func readPathValue(w http.ResponseWriter, r *http.Request) { _ = r.PathValue("id"); w.WriteHeader(204) }

// ---- variants (native + http-compatible where the lib has both) ----

type variant struct {
	name  string
	build func() runner
	regex bool // supports regex constraints (its constrained routes validate)
}

func buildStdlib() runner {
	m := http.NewServeMux()
	m.Handle("GET /ping", http.HandlerFunc(noop))
	m.Handle("GET /users/{id}", http.HandlerFunc(readPathValue))
	m.Handle("GET /users/{id}/posts/{pid}/comments/{cid}", http.HandlerFunc(noop))
	m.Handle("GET /static/{rest...}", http.HandlerFunc(noop))
	m.Handle("GET /c/{id}", http.HandlerFunc(readPathValue))
	m.Handle("GET /u/{id}", http.HandlerFunc(readPathValue))
	m.Handle("GET /blog/{date}/{slug}", http.HandlerFunc(noop))
	m.Handle("GET /a/{slug}", http.HandlerFunc(noop))
	for i := 0; i < many; i++ {
		m.Handle(fmt.Sprintf("GET /r%d/{id}", i), http.HandlerFunc(readPathValue))
	}
	return httpRunner{m}
}

func buildCompat() runner {
	r := router.NewRouter()
	r.Get("/ping", noop)
	r.Get("/users/{id}", func(w http.ResponseWriter, req *http.Request) { _ = router.Param(req, "id"); w.WriteHeader(204) })
	r.Get("/users/{id}/posts/{pid}/comments/{cid}", noop)
	r.Get("/static/*", noop)
	r.Get("/c/{id:"+reDigits+"}", func(w http.ResponseWriter, req *http.Request) { _ = router.Param(req, "id"); w.WriteHeader(204) })
	r.Get("/u/{id:"+reUUID+"}", func(w http.ResponseWriter, req *http.Request) { _ = router.Param(req, "id"); w.WriteHeader(204) })
	r.Get("/blog/{date:"+reDate+"}/{slug:"+reSlug+"}", noop)
	r.Get("/a/{slug:"+reSlug+"}", noop)
	for i := 0; i < many; i++ {
		r.Get(fmt.Sprintf("/r%d/{id}", i), func(w http.ResponseWriter, req *http.Request) { _ = router.Param(req, "id"); w.WriteHeader(204) })
	}
	return httpRunner{r}
}

func buildMux() runner {
	m := router.NewMux()
	read := func(w http.ResponseWriter, _ *http.Request, ps router.Params) { _ = ps.Get("id"); w.WriteHeader(204) }
	plain := func(w http.ResponseWriter, _ *http.Request, _ router.Params) { w.WriteHeader(204) }
	m.Get("/ping", plain)
	m.Get("/users/{id}", read)
	m.Get("/users/{id}/posts/{pid}/comments/{cid}", plain)
	m.Get("/static/*", plain)
	m.Get("/c/{id:"+reDigits+"}", read)
	m.Get("/u/{id:"+reUUID+"}", read)
	m.Get("/blog/{date:"+reDate+"}/{slug:"+reSlug+"}", plain)
	m.Get("/a/{slug:"+reSlug+"}", plain)
	for i := 0; i < many; i++ {
		m.Get(fmt.Sprintf("/r%d/{id}", i), read)
	}
	return httpRunner{m}
}

// buildMuxPlain registers the constrained paths as plain params, so mux can be
// compared with the routers that do not support regex.
func buildMuxPlain() runner {
	m := router.NewMux()
	read := func(w http.ResponseWriter, _ *http.Request, ps router.Params) { _ = ps.Get("id"); w.WriteHeader(204) }
	plain := func(w http.ResponseWriter, _ *http.Request, _ router.Params) { w.WriteHeader(204) }
	m.Get("/ping", plain)
	m.Get("/users/{id}", read)
	m.Get("/users/{id}/posts/{pid}/comments/{cid}", plain)
	m.Get("/static/*", plain)
	m.Get("/c/{id}", read)
	m.Get("/u/{id}", read)
	m.Get("/blog/{date}/{slug}", plain)
	m.Get("/a/{slug}", plain)
	for i := 0; i < many; i++ {
		m.Get(fmt.Sprintf("/r%d/{id}", i), read)
	}
	return httpRunner{m}
}

func buildChi() runner {
	r := chi.NewRouter()
	r.Get("/ping", noop)
	r.Get("/users/{id}", func(w http.ResponseWriter, req *http.Request) { _ = chi.URLParam(req, "id"); w.WriteHeader(204) })
	r.Get("/users/{id}/posts/{pid}/comments/{cid}", noop)
	r.Get("/static/*", noop)
	r.Get("/c/{id:"+reDigits+"}", func(w http.ResponseWriter, req *http.Request) { _ = chi.URLParam(req, "id"); w.WriteHeader(204) })
	r.Get("/u/{id:"+reUUID+"}", func(w http.ResponseWriter, req *http.Request) { _ = chi.URLParam(req, "id"); w.WriteHeader(204) })
	r.Get("/blog/{date:"+reDate+"}/{slug:"+reSlug+"}", noop)
	r.Get("/a/{slug:"+reSlug+"}", noop)
	for i := 0; i < many; i++ {
		r.Get(fmt.Sprintf("/r%d/{id}", i), func(w http.ResponseWriter, req *http.Request) { _ = chi.URLParam(req, "id"); w.WriteHeader(204) })
	}
	return httpRunner{r}
}

func buildHTTPRouter() runner {
	r := httprouter.New()
	h := func(w http.ResponseWriter, _ *http.Request, ps httprouter.Params) {
		_ = ps.ByName("id")
		w.WriteHeader(204)
	}
	plain := func(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) { w.WriteHeader(204) }
	r.GET("/ping", plain)
	r.GET("/users/:id", h)
	r.GET("/users/:id/posts/:pid/comments/:cid", plain)
	r.GET("/static/*filepath", plain)
	r.GET("/c/:id", h)
	r.GET("/u/:id", h)
	r.GET("/blog/:date/:slug", plain)
	r.GET("/a/:slug", plain)
	for i := 0; i < many; i++ {
		r.GET(fmt.Sprintf("/r%d/:id", i), h)
	}
	return httpRunner{r}
}

func buildHTTPRouterFunc() runner {
	r := httprouter.New()
	h := func(w http.ResponseWriter, req *http.Request) {
		_ = httprouter.ParamsFromContext(req.Context()).ByName("id")
		w.WriteHeader(204)
	}
	r.HandlerFunc("GET", "/ping", noop)
	r.HandlerFunc("GET", "/users/:id", h)
	r.HandlerFunc("GET", "/users/:id/posts/:pid/comments/:cid", noop)
	r.HandlerFunc("GET", "/static/*filepath", noop)
	r.HandlerFunc("GET", "/c/:id", h)
	r.HandlerFunc("GET", "/u/:id", h)
	r.HandlerFunc("GET", "/blog/:date/:slug", noop)
	r.HandlerFunc("GET", "/a/:slug", noop)
	for i := 0; i < many; i++ {
		r.HandlerFunc("GET", fmt.Sprintf("/r%d/:id", i), h)
	}
	return httpRunner{r}
}

func buildGin() runner {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.HandleMethodNotAllowed = true
	h := func(c *gin.Context) { _ = c.Param("id"); c.Status(204) }
	plain := func(c *gin.Context) { c.Status(204) }
	r.GET("/ping", plain)
	r.GET("/users/:id", h)
	r.GET("/users/:id/posts/:pid/comments/:cid", plain)
	r.GET("/static/*filepath", plain)
	r.GET("/c/:id", h)
	r.GET("/u/:id", h)
	r.GET("/blog/:date/:slug", plain)
	r.GET("/a/:slug", plain)
	for i := 0; i < many; i++ {
		r.GET(fmt.Sprintf("/r%d/:id", i), h)
	}
	return httpRunner{r}
}

func buildGinWrap() runner {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.HandleMethodNotAllowed = true
	w := gin.WrapF(noop)
	r.GET("/ping", w)
	r.GET("/users/:id", w)
	r.GET("/users/:id/posts/:pid/comments/:cid", w)
	r.GET("/static/*filepath", w)
	r.GET("/c/:id", w)
	r.GET("/u/:id", w)
	r.GET("/blog/:date/:slug", w)
	r.GET("/a/:slug", w)
	for i := 0; i < many; i++ {
		r.GET(fmt.Sprintf("/r%d/:id", i), w)
	}
	return httpRunner{r}
}

func buildEcho() runner {
	e := echo.New()
	h := func(c echo.Context) error { _ = c.Param("id"); return c.NoContent(204) }
	plain := func(c echo.Context) error { return c.NoContent(204) }
	e.GET("/ping", plain)
	e.GET("/users/:id", h)
	e.GET("/users/:id/posts/:pid/comments/:cid", plain)
	e.GET("/static/*", plain)
	e.GET("/c/:id", h)
	e.GET("/u/:id", h)
	e.GET("/blog/:date/:slug", plain)
	e.GET("/a/:slug", plain)
	for i := 0; i < many; i++ {
		e.GET(fmt.Sprintf("/r%d/:id", i), h)
	}
	return httpRunner{e}
}

func buildEchoWrap() runner {
	e := echo.New()
	h := echo.WrapHandler(http.HandlerFunc(noop))
	e.GET("/ping", h)
	e.GET("/users/:id", h)
	e.GET("/users/:id/posts/:pid/comments/:cid", h)
	e.GET("/static/*", h)
	e.GET("/c/:id", h)
	e.GET("/u/:id", h)
	e.GET("/blog/:date/:slug", h)
	e.GET("/a/:slug", h)
	for i := 0; i < many; i++ {
		e.GET(fmt.Sprintf("/r%d/:id", i), h)
	}
	return httpRunner{e}
}

func buildBunrouter() runner {
	r := bunrouter.New()
	h := bunrouter.HandlerFunc(func(w http.ResponseWriter, req bunrouter.Request) error {
		_ = req.Param("id")
		w.WriteHeader(204)
		return nil
	})
	plain := bunrouter.HTTPHandlerFunc(noop)
	r.GET("/ping", plain)
	r.GET("/users/:id", h)
	r.GET("/users/:id/posts/:pid/comments/:cid", plain)
	r.GET("/static/*path", plain)
	r.GET("/c/:id", h)
	r.GET("/u/:id", h)
	r.GET("/blog/:date/:slug", plain)
	r.GET("/a/:slug", plain)
	for i := 0; i < many; i++ {
		r.GET(fmt.Sprintf("/r%d/:id", i), h)
	}
	return httpRunner{r}
}

func buildBunrouterHTTP() runner {
	r := bunrouter.New()
	h := bunrouter.HTTPHandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = bunrouter.ParamsFromContext(req.Context()).ByName("id")
		w.WriteHeader(204)
	})
	plain := bunrouter.HTTPHandlerFunc(noop)
	r.GET("/ping", plain)
	r.GET("/users/:id", h)
	r.GET("/users/:id/posts/:pid/comments/:cid", plain)
	r.GET("/static/*path", plain)
	r.GET("/c/:id", h)
	r.GET("/u/:id", h)
	r.GET("/blog/:date/:slug", plain)
	r.GET("/a/:slug", plain)
	for i := 0; i < many; i++ {
		r.GET(fmt.Sprintf("/r%d/:id", i), h)
	}
	return httpRunner{r}
}

func buildGorilla() runner {
	r := mux.NewRouter()
	h := func(w http.ResponseWriter, req *http.Request) { _ = mux.Vars(req)["id"]; w.WriteHeader(204) }
	plain := noop
	r.HandleFunc("/ping", plain).Methods("GET")
	r.HandleFunc("/users/{id}", h).Methods("GET")
	r.HandleFunc("/users/{id}/posts/{pid}/comments/{cid}", plain).Methods("GET")
	r.PathPrefix("/static/").HandlerFunc(plain).Methods("GET")
	r.HandleFunc("/c/{id:"+reDigits+"}", h).Methods("GET")
	r.HandleFunc("/u/{id:"+reUUID+"}", h).Methods("GET")
	r.HandleFunc("/blog/{date:"+reDate+"}/{slug:"+reSlug+"}", plain).Methods("GET")
	r.HandleFunc("/a/{slug:"+reSlug+"}", plain).Methods("GET")
	for i := 0; i < many; i++ {
		r.HandleFunc(fmt.Sprintf("/r%d/{id}", i), h).Methods("GET")
	}
	return httpRunner{r}
}

func buildFiber() runner {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	h := func(c *fiber.Ctx) error { _ = c.Params("id"); return c.SendStatus(204) }
	plain := func(c *fiber.Ctx) error { return c.SendStatus(204) }
	app.Get("/ping", plain)
	app.Get("/users/:id", h)
	app.Get("/users/:id/posts/:pid/comments/:cid", plain)
	app.Get("/static/*", plain)
	app.Get("/c/:id", h)
	app.Get("/u/:id", h)
	app.Get("/blog/:date/:slug", plain)
	app.Get("/a/:slug", plain)
	for i := 0; i < many; i++ {
		app.Get(fmt.Sprintf("/r%d/:id", i), h)
	}
	return fiberRunner{app.Handler()}
}

func buildFiberHTTP() runner {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	h := adaptor.HTTPHandler(http.HandlerFunc(noop))
	app.Get("/ping", h)
	app.Get("/users/:id", h)
	app.Get("/users/:id/posts/:pid/comments/:cid", h)
	app.Get("/static/*", h)
	app.Get("/c/:id", h)
	app.Get("/u/:id", h)
	app.Get("/blog/:date/:slug", h)
	app.Get("/a/:slug", h)
	for i := 0; i < many; i++ {
		app.Get(fmt.Sprintf("/r%d/:id", i), h)
	}
	return fiberRunner{app.Handler()}
}

var variants = []variant{
	{"stdlib", buildStdlib, false},
	{"compat", buildCompat, true},
	{"mux", buildMux, true},
	{"mux-plain", buildMuxPlain, false},
	{"chi", buildChi, true},
	{"httprouter", buildHTTPRouter, false},
	{"httprouter-http", buildHTTPRouterFunc, false},
	{"gin", buildGin, false},
	{"gin-http", buildGinWrap, false},
	{"echo", buildEcho, false},
	{"echo-http", buildEchoWrap, false},
	{"bunrouter", buildBunrouter, false},
	{"bunrouter-http", buildBunrouterHTTP, false},
	{"gorilla", buildGorilla, true},
	{"fiber", buildFiber, false},
	{"fiber-http", buildFiberHTTP, false},
}

var scenarios = []struct {
	name, method, path string
	want               int
}{
	{"static", "GET", pStatic, 204},
	{"param", "GET", pParam, 204},
	{"deep", "GET", pDeep, 204},
	{"wildcard", "GET", pWild, 204},
	{"many", "GET", pMany, 204},
	{"longparam", "GET", pLong, 204},
	{"miss", "GET", pMiss, 404},
	{"405", "DELETE", pStatic, 405},
}

// constrainedScenarios run only within a category: regex-capable routers
// validate them, the others register a plain param (see BenchmarkConstrained*
// and the README).
var constrainedScenarios = []struct {
	name, method, path string
	want               int
}{
	{"constrained", "GET", pConstrained, 204},
	{"constrained-uuid", "GET", pUUID, 204},
	{"constrained-blog", "GET", pBlog, 204},
	{"constrained-slug", "GET", pSlug, 204},
}

func BenchmarkRouters(b *testing.B) {
	for _, sc := range scenarios {
		for _, v := range variants {
			b.Run(sc.name+"/"+v.name, func(b *testing.B) {
				v.build().run(b, sc.method, sc.path, sc.want)
			})
		}
	}
}

// BenchmarkConstrained: routers that support regex constraints, validating them.
func BenchmarkConstrained(b *testing.B) {
	for _, sc := range constrainedScenarios {
		for _, v := range variants {
			if !v.regex {
				continue
			}
			b.Run(sc.name+"/"+v.name, func(b *testing.B) {
				v.build().run(b, sc.method, sc.path, sc.want)
			})
		}
	}
}

// BenchmarkConstrainedPlain: routers without regex support (plus our mux with a
// plain param), all registering the constrained paths as plain params — the
// routing cost without validation.
func BenchmarkConstrainedPlain(b *testing.B) {
	for _, sc := range constrainedScenarios {
		for _, v := range variants {
			if v.regex {
				continue
			}
			b.Run(sc.name+"/"+v.name, func(b *testing.B) {
				v.build().run(b, sc.method, sc.path, sc.want)
			})
		}
	}
}

// BenchmarkParallel measures throughput under b.RunParallel (GOMAXPROCS
// goroutines). Each goroutine has its own request and writer.
func BenchmarkParallel(b *testing.B) {
	scs := []struct {
		name, method, path string
		want               int
	}{
		{"static", "GET", pStatic, 204},
		{"param", "GET", pParam, 204},
		{"deep", "GET", pDeep, 204},
		{"constrained-uuid", "GET", pUUID, 204},
		{"miss", "GET", pMiss, 404},
	}
	for _, sc := range scs {
		for _, v := range variants {
			b.Run(sc.name+"/"+v.name, func(b *testing.B) {
				v.build().runParallel(b, sc.method, sc.path, sc.want)
			})
		}
	}
}

// BenchmarkQuick is a small subset for a fast sanity run.
func BenchmarkQuick(b *testing.B) {
	keep := map[string]bool{"param": true, "deep": true, "miss": true}
	for _, sc := range scenarios {
		if !keep[sc.name] {
			continue
		}
		for _, v := range variants {
			b.Run(sc.name+"/"+v.name, func(b *testing.B) {
				v.build().run(b, sc.method, sc.path, sc.want)
			})
		}
	}
	b.Run("constrained-uuid/mux", func(b *testing.B) { buildMux().run(b, "GET", pUUID, 204) })
	b.Run("constrained-uuid/mux-plain", func(b *testing.B) { buildMuxPlain().run(b, "GET", pUUID, 204) })
}
