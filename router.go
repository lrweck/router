// Package router gives you the Chi API (routes, groups, params, sub-routers)
// in two engines. The API, the pattern syntax and the behavioral test suite are
// derived from Chi (github.com/go-chi/chi, MIT); see NOTICE.md for the full
// attribution and licenses.
//
//   - [Compat]: Chi-compatible handlers (http.HandlerFunc), backed by
//     net/http.ServeMux, so it keeps the stdlib's routing semantics. This is
//     the default; [NewRouter] is an alias for [NewCompat].
//   - [Mux]: typed handlers (params as arguments), backed by a segment trie.
//     This is the fast path: zero allocations per request.
//
// [Router] is the registration interface both closures use, like chi.Router.
//
// Where Chi and the stdlib disagree, the stdlib wins (Compat):
//
//   - path canonicalization ("//", ".") redirects, like net/http;
//   - "/x" with only "/x/" registered gets net/http's redirect (301/307,
//     depending on the Go version), not Chi's 404;
//   - param values are unescaped, as net/http.PathValue returns them;
//   - the 405 Allow header is net/http's single comma-joined value;
//   - HEAD is served from GET routes, no Head/GetHead needed.
//
// See stdlib_behavior_test.go for each of these pinned to the stdlib behavior.
//
// Chi:
//
//	r := chi.NewRouter()
//	r.Use(middleware.Logger)
//	r.Get("/users/{id:[0-9]+}", h)
//
// router:
//
//	r := router.NewRouter()
//	r.Use(Logger)
//	r.Get("/users/{id:[0-9]+}", h)
//	id := router.Param(r, "id") // primary accessor; URLParam is an alias
//
// Typed (fast path, no per-request context):
//
//	m := router.NewMux()
//	m.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request, ps router.Params) {
//		id := ps.Get("id")
//	})
//
// Additions over the stdlib (they don't change the behavior above): {name:expr}
// constraints compiled without the regexp package (see constraint.go), and Chi
// pattern sugar ({a}-{b}, prefix*) desugared to stdlib patterns.
//
// # Fast path and the routing Context
//
// In Compat, plain routes (static segments and whole-segment params with
// stdlib-safe names) keep the user's name as the stdlib placeholder, so
// net/http sets r.PathValue natively and no per-request routing Context is
// allocated. Read params with [Param] (which falls back to r.PathValue) or the
// matched pattern with [http.Request.Pattern]. In that case [RouteContext]
// returns nil and Context.RoutePattern() is empty.
//
// Routes that need request-scoped state create the Context lazily: mounts
// (to accumulate params across sub-routers), wildcards, mixed segments
// ({a}-{b}), and params renamed to a stdlib placeholder. For those,
// [RouteContext], [URLParamFromCtx] and Context.RoutePattern() work as in Chi.
package router

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
)

// Middleware matches both Chi (func(http.Handler) http.Handler) and stdlib,
// so existing Chi middlewares plug in directly.
type Middleware = func(http.Handler) http.Handler

// Middlewares is a slice of middlewares, like chi.Middlewares.
type Middlewares []func(http.Handler) http.Handler

// MethodQuery is the QUERY HTTP method (like chi, until net/http defines it).
const MethodQuery = "QUERY"

// RegisterMethod exists for source compatibility with Chi. Unlike Chi,
// Method/MethodFunc already accept any method here, so it does nothing.
func RegisterMethod(string) {}

// Compat is a Chi-style router backed by *http.ServeMux.
type Compat struct {
	mux  *http.ServeMux
	root *Compat

	inline []Middleware // With/Group/Route-level middlewares, accumulated

	// Root-only state below, accessed via r.root.
	use              []Middleware
	frozen           bool // set on first route/With/Group/Route/Mount
	routes           []*route
	byScore          []*route            // routes sorted by score desc, rebuilt on register
	slots            map[string]*slot    // stdlib pattern -> candidate routes sharing it
	shapeNames       map[string][]string // pattern shape -> stdlib placeholder per segment
	chain            http.Handler        // built at Use time; read-only while serving
	catchallOnce     sync.Once
	missFirst        map[string]struct{} // first static segments, for the miss filter
	notFound         http.Handler
	methodNotAllowed http.Handler
}

var _ http.Handler = (*Compat)(nil)

// NewRouter creates a new compatible router (net/http.ServeMux semantics),
// like chi.NewRouter. NewCompat is an explicit alias.
func NewRouter() *Compat { return NewCompat() }

// NewCompat creates a new compatible router, like chi.NewRouter.
func NewCompat() *Compat {
	r := &Compat{mux: http.NewServeMux(), slots: map[string]*slot{}, shapeNames: map[string][]string{}}
	r.root = r
	return r
}

// Router is the registration interface shared by the compatible router, so
// closures receive an interface (like chi.Router) instead of the concrete type.
type Router interface {
	http.Handler
	Routes
	Use(mws ...Middleware)
	With(mws ...Middleware) Router
	Group(fn func(Router)) Router
	Route(pattern string, fn func(Router)) Router
	Mount(pattern string, h http.Handler)
	Handle(pattern string, h http.Handler)
	HandleFunc(pattern string, fn http.HandlerFunc)
	Any(pattern string, fn http.HandlerFunc)
	Method(method, pattern string, h http.Handler)
	MethodFunc(method, pattern string, fn http.HandlerFunc)
	Connect(pattern string, fn http.HandlerFunc)
	Delete(pattern string, fn http.HandlerFunc)
	Get(pattern string, fn http.HandlerFunc)
	Head(pattern string, fn http.HandlerFunc)
	Options(pattern string, fn http.HandlerFunc)
	Patch(pattern string, fn http.HandlerFunc)
	Post(pattern string, fn http.HandlerFunc)
	Put(pattern string, fn http.HandlerFunc)
	Query(pattern string, fn http.HandlerFunc)
	Trace(pattern string, fn http.HandlerFunc)
	NotFound(fn http.HandlerFunc)
	MethodNotAllowed(fn http.HandlerFunc)
}

var _ Router = (*Compat)(nil)

// Use appends middlewares, like chi. On a root router it panics if routes
// were already registered (same rule as Chi); on a Route/With sub-router it
// scopes to that sub-router.
// freeze marks the router as registered and builds the middleware chain once
// (like Chi's updateRouteHandler), so middleware constructors run a single time
// and the chain is read-only while serving.
func (r *Compat) freeze() {
	root := r.root
	root.frozen = true
	if root.chain == nil {
		root.chain = chainMiddlewares(root.use, http.HandlerFunc(root.dispatch))
	}
}

func (r *Compat) Use(mws ...Middleware) {
	if r.root == r {
		if r.frozen {
			panic("chi: all middlewares must be defined before routes on a mux")
		}
		r.use = append(r.use, mws...)
		return
	}
	r.inline = append(r.inline, mws...)
}

// With returns a sub-router with extra inline middlewares, like chi.
func (r *Compat) With(mws ...Middleware) Router {
	r.root.freeze()
	return &Compat{
		mux:    r.mux,
		root:   r.root,
		inline: append(slices.Clone(r.inline), mws...),
	}
}

// Group scopes middlewares without a path prefix, like chi.
func (r *Compat) Group(fn func(Router)) Router {
	im := r.With()
	if fn != nil {
		fn(im)
	}
	return im
}

// Route creates a sub-router mounted at pattern, like chi.Route. It returns
// the sub-router so middleware can be added inside the closure.
func (r *Compat) Route(pattern string, fn func(Router)) Router {
	if fn == nil {
		panic(fmt.Sprintf("chi: attempting to Route() a nil subrouter on '%s'", pattern))
	}
	sub := NewCompat()
	fn(sub)
	r.Mount(pattern, sub)
	return sub
}

// Mount attaches another handler along pattern, like chi. A *Compat mounted
// this way keeps its own middleware stack and params; parent URL params stay
// visible inside it via Param.
func (r *Compat) Mount(pattern string, h http.Handler) {
	if h == nil {
		panic(fmt.Sprintf("chi: attempting to Mount() a nil handler on '%s'", pattern))
	}
	if pattern == "" || pattern[0] != '/' {
		panic(fmt.Sprintf("chi: routing pattern must begin with '/' in '%s'", pattern))
	}
	if sub, ok := h.(*Compat); ok && sub.mux == r.mux {
		panic(fmt.Sprintf("chi: attempting to Mount() a router onto itself on '%s'", pattern))
	}
	full := pattern
	for _, rt := range r.root.routes {
		if rt.isMount && rt.mountBase == full {
			panic(fmt.Sprintf("chi: attempting to Mount() a handler on an existing path, '%s'", pattern))
		}
	}
	r.root.freeze()

	if sub, ok := h.(*Compat); ok {
		if sub.notFound == nil && r.root.notFound != nil {
			sub.setNotFound(r.root.notFound)
		}
		if sub.methodNotAllowed == nil && r.root.methodNotAllowed != nil {
			sub.setMethodNotAllowed(r.root.methodNotAllowed)
		}
	}

	base := strings.TrimSuffix(full, "/")
	if base == "" {
		base = "/"
	}
	strip := r.mountStrip(h)
	var subRoutes Routes
	if rs, ok := h.(Routes); ok {
		subRoutes = rs
	}
	segs := mustParse(base)
	baseSegs := segs
	r.root.applyShapeNames(baseSegs, strings.HasSuffix(full, "/"))
	pat := base + "/*"
	if base == "/" {
		pat = "/*"
	}
	mrt := &route{
		pattern:   pat,
		mountBase: full,
		segs:      append(segs, segment{kind: segWild, ph: "rest", direct: true}),
		isMount:   true,
		needsCtx:  true, // accumulate params/patterns across sub-routers
		endpoint:  strip,
		inline:    slices.Clone(r.inline),
		owner:     r,
		sub:       subRoutes,
		score:     -1000,
	}
	inner := mrt.inner()
	switch {
	case full == "/":
		// Mounting at the root: the wildcard captures the whole path.
		r.muxRoute(canonicalPath(mrt.segs), mrt, inner)
	case strings.HasSuffix(full, "/"):
		// Explicit trailing slash: no bare stub, like Chi (Route("/x/")
		// must not answer "/x").
		r.muxRoute(canonicalPath(baseSegs)+"/{$}", mrt, inner)
		r.muxRoute(canonicalPath(mrt.segs), mrt, inner)
	default:
		r.muxRoute(canonicalPath(baseSegs), mrt, inner)
		r.muxRoute(canonicalPath(mrt.segs), mrt, inner)
	}
	r.root.addRoute(mrt)
}

// mountStrip rewrites the request path to the mount remainder. The remainder
// comes from the mount's captured wildcard (the literal mount pattern may
// contain placeholders, so string-trimming it would not work), normalized to
// "/". RawPath is dropped: routing is on the decoded path.
func (r *Compat) mountStrip(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Entering a mount clears the connecting wildcard, like Chi: the
		// sub-router's own params replace it.
		if rctx := RouteContext(req.Context()); rctx != nil {
			rctx.params.clearWild()
		}
		rest := req.PathValue("*")
		if rest == "" {
			rest = "/"
		} else {
			rest = "/" + rest
		}
		r2 := new(http.Request)
		*r2 = *req
		u2 := new(url.URL)
		*u2 = *req.URL
		u2.Path = rest
		u2.RawPath = ""
		r2.URL = u2
		r2.SetPathValue("*", "")
		r2.SetPathValue("rest", "")
		h.ServeHTTP(w, r2)
	})
}

// Any registers pattern for all HTTP methods (shorthand for Handle).
func (r *Compat) Any(pattern string, fn http.HandlerFunc) {
	r.Handle(pattern, fn)
}

// Handle registers pattern for all methods, like chi. A "METHOD /path" prefix
// selects a single method.
func (r *Compat) Handle(pattern string, h http.Handler) {
	if i := strings.IndexAny(pattern, " \t"); i >= 0 {
		r.Method(pattern[:i], strings.TrimLeft(pattern[i+1:], " \t"), h)
		return
	}
	r.register("", pattern, h)
}

// HandleFunc registers pattern for all methods, like chi.
func (r *Compat) HandleFunc(pattern string, fn http.HandlerFunc) {
	r.Handle(pattern, fn)
}

// Method registers pattern for one HTTP method, like chi. Any method name is
// accepted (superset of Chi, which requires RegisterMethod first).
func (r *Compat) Method(method, pattern string, h http.Handler) {
	r.register(strings.ToUpper(method), pattern, h)
}

// MethodFunc registers pattern for one HTTP method, like chi.
func (r *Compat) MethodFunc(method, pattern string, fn http.HandlerFunc) {
	r.Method(method, pattern, fn)
}

// Connect registers a CONNECT route, like chi.
func (r *Compat) Connect(pattern string, fn http.HandlerFunc) {
	r.register(http.MethodConnect, pattern, fn)
}

// Delete registers a DELETE route, like chi.
func (r *Compat) Delete(pattern string, fn http.HandlerFunc) {
	r.register(http.MethodDelete, pattern, fn)
}

// Get registers a GET route, like chi.
func (r *Compat) Get(pattern string, fn http.HandlerFunc) {
	r.register(http.MethodGet, pattern, fn)
}

// Head registers a HEAD route, like chi.
func (r *Compat) Head(pattern string, fn http.HandlerFunc) {
	r.register(http.MethodHead, pattern, fn)
}

// Options registers an OPTIONS route, like chi.
func (r *Compat) Options(pattern string, fn http.HandlerFunc) {
	r.register(http.MethodOptions, pattern, fn)
}

// Patch registers a PATCH route, like chi.
func (r *Compat) Patch(pattern string, fn http.HandlerFunc) {
	r.register(http.MethodPatch, pattern, fn)
}

// Post registers a POST route, like chi.
func (r *Compat) Post(pattern string, fn http.HandlerFunc) {
	r.register(http.MethodPost, pattern, fn)
}

// Put registers a PUT route, like chi.
func (r *Compat) Put(pattern string, fn http.HandlerFunc) {
	r.register(http.MethodPut, pattern, fn)
}

// Query registers a QUERY route, like chi.
func (r *Compat) Query(pattern string, fn http.HandlerFunc) {
	r.register(MethodQuery, pattern, fn)
}

// Trace registers a TRACE route, like chi.
func (r *Compat) Trace(pattern string, fn http.HandlerFunc) {
	r.register(http.MethodTrace, pattern, fn)
}

// NotFound sets a custom 404 handler, like chi. Middlewares from an inline
// With()/Group() router wrap the handler, and it propagates into mounted
// sub-routers that don't define their own, so nested misses bubble up.
func (r *Compat) NotFound(fn http.HandlerFunc) {
	r.root.setNotFound(r.wrapInline(fn))
}

// wrapInline applies the router's inline middlewares around a handler, like
// Chi's NotFound/MethodNotAllowed on an inline mux.
func (r *Compat) wrapInline(fn http.HandlerFunc) http.Handler {
	var h http.Handler = fn
	for _, v := range slices.Backward(r.inline) {
		h = v(h)
	}
	return h
}

func (r *Compat) setNotFound(fn http.Handler) {
	r.root.notFound = fn
	for _, rt := range r.root.routes {
		if !rt.isMount {
			continue
		}
		if sub, ok := rt.sub.(*Compat); ok && sub.notFound == nil {
			sub.setNotFound(fn)
		}
	}
}

// MethodNotAllowed sets a custom 405 handler, like chi, propagating into
// mounted sub-routers without one of their own.
func (r *Compat) MethodNotAllowed(fn http.HandlerFunc) {
	r.root.setMethodNotAllowed(r.wrapInline(fn))
}

func (r *Compat) setMethodNotAllowed(fn http.Handler) {
	r.root.methodNotAllowed = fn
	for _, rt := range r.root.routes {
		if !rt.isMount {
			continue
		}
		if sub, ok := rt.sub.(*Compat); ok && sub.methodNotAllowed == nil {
			sub.setMethodNotAllowed(fn)
		}
	}
}

// Middlewares returns the middleware stack in use, like chi.
func (r *Compat) Middlewares() Middlewares {
	if r.root == r {
		return slices.Clone(r.use)
	}
	return slices.Clone(r.inline)
}

// addRoute appends a route and keeps the score-sorted index fresh.
// Registration-time cost; Find walks the index without sorting.
func (r *Compat) addRoute(rt *route) {
	r.routes = append(r.routes, rt)
	r.byScore = append(r.byScore, rt)
	slices.SortStableFunc(r.byScore, func(a, b *route) int { return b.score - a.score })
}

func (r *Compat) register(method, pattern string, h http.Handler) {
	if pattern == "" || pattern[0] != '/' {
		panic(fmt.Sprintf("chi: routing pattern must begin with '/' in '%s'", pattern))
	}
	r.root.freeze()
	full := pattern
	segs := mustParse(full)
	rt := &route{
		method:   method,
		pattern:  full,
		segs:     segs,
		trailing: strings.HasSuffix(full, "/") && full != "/",
		endpoint: h,
		inline:   slices.Clone(r.inline),
		owner:    r,
		score:    scoreSegs(segs),
	}
	rt.assignNames(r.root)
	rt.simple = rt.computeSimple()
	rt.needsCtx = rt.computeNeedsCtx()
	inner := rt.inner()
	for _, p := range rt.stdPatterns(method) {
		r.muxRoute(p, rt, inner)
	}
	r.root.addRoute(rt)
}

// muxRoute registers or joins the slot for one stdlib pattern. Several Chi
// routes can desugar to the same pattern (same shape, different constraint);
// they share a slot and are tried in specificity order, so stdlib's conflict
// panic never fires for a valid Chi route set.
func (r *Compat) muxRoute(pattern string, rt *route, inner http.Handler) {
	root := r.root
	sl := root.slots[pattern]
	if sl == nil {
		shape := pattern
		if !strings.HasPrefix(pattern, "/") {
			if _, after, ok := strings.Cut(pattern, " "); ok {
				shape = after
			}
		}
		sl = &slot{root: root, pattern: pattern, shape: shape}
		root.slots[pattern] = sl
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					panic(fmt.Sprintf("router: cannot register %q (method %q): %v", rt.pattern, rt.method, rec))
				}
			}()
			root.mux.Handle(pattern, sl)
		}()
	}
	sl.add(rt, inner)
}

// ServeHTTP dispatches via the stdlib mux. Root Use-middlewares wrap
// everything (including 404/405), like Chi. The routing Context is created
// lazily by routes that need it (see route.needsCtx); plain routes read params
// from r.PathValue with no per-request context allocation.
func (r *Compat) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	root := r.root
	if root != r {
		root.ServeHTTP(w, req)
		return
	}
	chain := root.chain
	if chain == nil {
		chain = http.HandlerFunc(root.dispatch)
	}
	chain.ServeHTTP(w, req)
}

func chainMiddlewares(mws []Middleware, h http.Handler) http.Handler {
	for _, mw := range slices.Backward(mws) {
		h = mw(h)
	}
	return h
}

// dispatch matches with the stdlib mux. mux.Handler must not be used to serve
// (it skips PathValue injection), only as a match probe.
func (r *Compat) dispatch(w http.ResponseWriter, req *http.Request) {
	r.ensureCatchall()
	if r.notFound == nil && r.methodNotAllowed == nil {
		r.mux.ServeHTTP(w, req)
		return
	}
	if _, pat := r.mux.Handler(req); pat != "" {
		r.mux.ServeHTTP(w, req)
		return
	}
	r.missServe(w, req)
}

// ensureCatchall registers a least-specific "/" handler so a request that
// matches no route lands here instead of in net/http's 404 path, which walks
// the whole tree (matchingMethods) just to build the Allow header. We already
// do that walk (missAllow) and produce the same 404/405. Skipped when the user
// registered a "/" route of their own (which is itself a catch-all).
func (r *Compat) ensureCatchall() {
	r.catchallOnce.Do(func() {
		hasRoot := false
		filter := map[string]struct{}{}
		filterOK := true
		for _, rt := range r.routes {
			segs := rt.segs
			if rt.isMount {
				segs = rt.segs[:len(rt.segs)-1] // drop the mount wildcard
			}
			if len(segs) == 0 {
				// A root route ("/") matches everything: no catch-all and no
				// first-segment filter.
				hasRoot, filterOK = true, false
				continue
			}
			if segs[0].kind != segStatic {
				filterOK = false
				continue
			}
			filter[segs[0].text] = struct{}{}
		}
		if filterOK {
			r.missFirst = filter
		}
		if !hasRoot {
			r.mux.Handle("/", http.HandlerFunc(r.missServe))
		}
	})
}

// mayMatchFirst reports whether some route could match path, using the
// first static segment. Only meaningful when r.missFirst is set (every route
// starts with a static segment and no route is a root catch-all).
func (r *Compat) mayMatchFirst(path string) bool {
	seg := path
	if len(seg) > 0 && seg[0] == '/' {
		seg = seg[1:]
	}
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		seg = seg[:i]
	}
	_, ok := r.missFirst[seg]
	return ok
}

// missServe answers 405 when some route matches the path under another
// method, else 404.
func (r *Compat) missServe(w http.ResponseWriter, req *http.Request) {
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	// Fast negative: no route can match this first segment, so it is a plain
	// 404 — skip the O(routes) allowed-methods walk.
	if r.missFirst != nil && !r.mayMatchFirst(path) {
		if r.notFound != nil {
			r.notFound.ServeHTTP(w, req)
			return
		}
		http.NotFound(w, req)
		return
	}
	// net/http's matchingMethods probes the path and, when it lacks one, the
	// trailing-slash variant too, then sorts the result. Match that.
	allow := r.missAllow(path)
	if !strings.HasSuffix(path, "/") {
		allow = append(allow, r.missAllow(path+"/")...)
	}
	slices.Sort(allow)
	allow = slices.Compact(allow)
	if len(allow) > 0 {
		if r.methodNotAllowed != nil {
			r.methodNotAllowed.ServeHTTP(w, req)
			return
		}
		// Mirror net/http's own 405 exactly (same Allow format, same Error).
		w.Header().Set("Allow", strings.Join(allow, ", "))
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	if r.notFound != nil {
		r.notFound.ServeHTTP(w, req)
		return
	}
	http.NotFound(w, req)
}

// missAllow returns methods of routes matching path (validators included),
// descending into mounted sub-routers. Shape-only, no param collection.
func (r *Compat) missAllow(path string) []string {
	var allow []string
	for _, rt := range r.routes {
		rem, ok := rt.matchShape(path)
		if !ok {
			continue
		}
		if rt.isMount {
			if sub, ok := rt.sub.(*Compat); ok {
				allow = append(allow, sub.missAllow(rem)...)
			}
			continue
		}
		if rt.method == "" {
			continue // all-method routes can't 405: the mux would have matched
		}
		if !slices.Contains(allow, rt.method) {
			allow = append(allow, rt.method)
		}
	}
	if slices.Contains(allow, http.MethodGet) && !slices.Contains(allow, http.MethodHead) {
		allow = append(allow, http.MethodHead) // stdlib serves HEAD from GET
	}
	return allow
}

// Routes returns the routing table, like chi (for docgen/Walk).
func (r *Compat) Routes() []Route {
	var out []Route
	byPattern := map[string]int{}
	for _, rt := range r.root.routes {
		if !r.owns(rt) {
			continue
		}
		if rt.isMount {
			hs := map[string]http.Handler{}
			if rt.endpoint != nil {
				hs["*"] = rt.endpoint
			}
			out = append(out, Route{SubRoutes: rt.sub, Handlers: hs, Pattern: rt.mountBase})
			continue
		}
		key := rt.method
		if key == "" {
			key = "*"
		}
		if i, ok := byPattern[rt.pattern]; ok {
			out[i].Handlers[key] = rt.endpoint
			continue
		}
		byPattern[rt.pattern] = len(out)
		out = append(out, Route{Handlers: map[string]http.Handler{key: rt.endpoint}, Pattern: rt.pattern})
	}
	return out
}

func (r *Compat) owns(rt *route) bool {
	return rt.owner != nil && rt.owner.root == r.root
}

// Match searches for a handler matching method/path, like chi (no execution).
func (r *Compat) Match(rctx *Context, method, path string) bool {
	return r.Find(rctx, method, path) != ""
}

// Find returns the pattern matching method/path, like chi.
func (r *Compat) Find(rctx *Context, method, path string) string {
	if rctx == nil {
		rctx = NewRouteContext()
	}
	if method == http.MethodHead {
		method = http.MethodGet // stdlib serves HEAD from GET routes
	}
	for _, rt := range r.orderedRoutes() {
		if rt.isMount {
			m, ok := rt.matchPath(path)
			if !ok {
				continue
			}
			sub, ok := rt.sub.(*Compat)
			if !ok {
				rctx.RoutePatterns = append(rctx.RoutePatterns, rt.pattern)
				return rt.pattern
			}
			beforeParams := rctx.params.n
			beforePats := len(rctx.RoutePatterns)
			rctx.RoutePatterns = append(rctx.RoutePatterns, rt.pattern)
			if subpat := sub.Find(rctx, method, m.remainder); subpat != "" {
				return strings.TrimSuffix(rt.pattern, "/*") + subpat
			}
			rctx.params.n = beforeParams
			rctx.RoutePatterns = rctx.RoutePatterns[:beforePats]
			continue
		}
		if rt.method != "" && rt.method != method {
			continue
		}
		m, ok := rt.matchPath(path)
		if !ok {
			continue
		}
		for _, kv := range m.params {
			rctx.params.Add(kv[0], kv[1])
		}
		rctx.RoutePatterns = append(rctx.RoutePatterns, rt.pattern)
		return rt.pattern
	}
	return ""
}

func (r *Compat) orderedRoutes() []*route {
	owned := make([]*route, 0, len(r.root.byScore))
	for _, rt := range r.root.byScore {
		if r.owns(rt) {
			owned = append(owned, rt)
		}
	}
	return owned
}

// Route describes one routing handler, like chi.Route.
type Route struct {
	SubRoutes Routes
	Handlers  map[string]http.Handler
	Pattern   string
}

// Routes is the traversal interface, like chi.Routes.
type Routes interface {
	Routes() []Route
	Middlewares() Middlewares
	Match(rctx *Context, method, path string) bool
	Find(rctx *Context, method, path string) string
}

// WalkFunc is called per method/route, like chi.WalkFunc.
type WalkFunc func(method string, route string, handler http.Handler, middlewares ...func(http.Handler) http.Handler) error

// Walk walks routes, like chi.Walk (works on any Routes, including chi's).
func Walk(r Routes, fn WalkFunc) error {
	return walk(r, fn, "", nil)
}

func walk(r Routes, fn WalkFunc, parent string, parentMw Middlewares) error {
	if rr, ok := r.(*Compat); ok {
		return rr.walkInternal(fn, parent, parentMw)
	}
	mws := append(slices.Clone(parentMw), r.Middlewares()...)
	for _, rt := range r.Routes() {
		if rt.SubRoutes != nil {
			if err := walk(rt.SubRoutes, fn, parent+trimMount(rt.Pattern), mws); err != nil {
				return err
			}
			continue
		}
		for method, h := range rt.Handlers {
			if method == "*" {
				continue
			}
			if err := fn(method, parent+rt.Pattern, h, mws...); err != nil {
				return err
			}
		}
	}
	return nil
}

func trimMount(p string) string {
	return strings.TrimSuffix(p, "/*")
}

func (r *Compat) walkInternal(fn WalkFunc, parent string, parentMw Middlewares) error {
	base := append(slices.Clone(parentMw), r.root.use...)
	for _, rt := range r.root.routes {
		if !r.owns(rt) {
			continue
		}
		mws := append(slices.Clone(base), rt.inline...)
		if rt.isMount {
			if rt.sub != nil {
				if sub, ok := rt.sub.(*Compat); ok {
					if err := sub.walkInternal(fn, parent+rt.mountBase, mws); err != nil {
						return err
					}
					continue
				}
			}
			continue
		}
		method := rt.method
		if method == "" {
			method = "*"
		}
		if method == "*" {
			continue
		}
		if err := fn(method, parent+rt.pattern, rt.endpoint, mws...); err != nil {
			return err
		}
	}
	return nil
}

// FileServer serves files under path, like chi.FileServer.
func FileServer(r *Compat, path string, root http.FileSystem) {
	if strings.ContainsAny(path, "{}*") {
		panic("chi: FileServer does not permit any URL parameters.")
	}
	if path != "/" && !strings.HasSuffix(path, "/") {
		r.Get(path, http.RedirectHandler(path+"/", http.StatusMovedPermanently).ServeHTTP)
		path += "/"
	}
	r.Get(path+"*", func(w http.ResponseWriter, req *http.Request) {
		http.StripPrefix(strings.TrimSuffix(path, "*"), http.FileServer(root)).ServeHTTP(w, req)
	})
}

// slot is one registered stdlib ServeMux pattern, shared by every Chi route
// that desugars to it. Routes that differ only by constraint (e.g.
// /articles/{slug:[a-z-]+} and /articles/{month}-{day}-{year}) cannot be
// expressed as distinct stdlib patterns, so they become candidates here and
// the first one whose validators accept the request serves.
type slot struct {
	root    *Compat
	pattern string // full stdlib pattern, possibly "METHOD /path"
	shape   string // pattern without the method prefix
	cands   []*candidate
}

type candidate struct {
	rt    *route
	inner http.Handler
	sig   string
}

// add registers a route in the slot. A route with the same signature is the
// same route and replaces the previous handler (last registration wins, like
// Chi); a different signature is a fallback, kept in specificity order.
func (s *slot) add(rt *route, inner http.Handler) {
	sig := rt.signature()
	for i, c := range s.cands {
		if c.sig == sig {
			s.cands[i] = &candidate{rt: rt, inner: inner, sig: sig}
			return
		}
	}
	s.cands = append(s.cands, &candidate{rt: rt, inner: inner, sig: sig})
	slices.SortStableFunc(s.cands, func(a, b *candidate) int { return b.rt.score - a.rt.score })
}

// ServeHTTP tries each candidate until one validates and serves. If this is a
// method-specific slot and every candidate rejects the request, it falls back
// to the all-methods routes of the same shape (Chi's mALL/mSTUB routes) — that
// is how a mounted sub-router still receives requests that a sibling
// constrained GET route did not accept. Otherwise it falls through to the
// root's 404/405 handling.
func (s *slot) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if s.tryCands(w, req) {
		return
	}
	if s.shape != s.pattern {
		if alt := s.root.slots[s.shape]; alt != nil {
			if alt.tryCands(w, req) {
				return
			}
		}
	}
	s.root.missServe(w, req)
}

func (s *slot) tryCands(w http.ResponseWriter, req *http.Request) bool {
	for _, c := range s.cands {
		if c.rt.try(w, req, c.inner) {
			return true
		}
	}
	return false
}
