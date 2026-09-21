package router

// Mux: a hand-rolled routing core for when you need to beat net/http's mux.
//
// The typed-handler idea — a tree router whose handler takes the path
// parameters as arguments, so a match allocates nothing — is httprouter's
// (github.com/julienschmidt/httprouter, BSD-3-Clause). The static-first
// matching strategy was informed by matchit (github.com/ibraheemdev/matchit,
// MIT, the router behind axum). Both were reimplemented, not copied; see
// NOTICE.md.
//
// The compatible API (Compat, backed by ServeMux) stays the default. This is
// the opt-in fast path: a segment trie with typed handlers, so path parameters
// arrive as arguments and no per-request Context or PathValue is allocated.
// Compatible handlers are built on top of the typed ones (the reverse of how
// the ServeMux-backed Compat works), like httprouter's 3-arg core with
// http.Handler adapters.
//
// Reuses the same pattern syntax as the rest of the package (see pattern.go):
// {name}, {name:constraint}, {a}-{b}, prefix*, wildcards.

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// TypedHandler receives the matched path parameters directly.
type TypedHandler func(w http.ResponseWriter, r *http.Request, ps Params)

const numMethods = 9

func methodIndex(m string) int {
	switch m {
	case http.MethodGet:
		return 0
	case http.MethodPost:
		return 1
	case http.MethodPut:
		return 2
	case http.MethodDelete:
		return 3
	case http.MethodPatch:
		return 4
	case http.MethodHead:
		return 5
	case http.MethodOptions:
		return 6
	case http.MethodConnect:
		return 7
	case http.MethodTrace:
		return 8
	}
	return -1
}

var methodNames = [numMethods]string{
	http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete,
	http.MethodPatch, http.MethodHead, http.MethodOptions,
	http.MethodConnect, http.MethodTrace,
}

type tnode struct {
	// Static children. A single child (the common case) is stored inline; two
	// or more go into a sorted slice, with a first-byte bucket index once there
	// are enough of them to make it worth the memory. Internal nodes stay small
	// because the handler table lives in *leaf, only on terminals.
	one   staticChild
	kids  []staticChild
	first []int16

	params []*tnode // {name} / {a}-{b} edges, constrained first
	wild   *tnode   // * edge
	leaf   *leaf    // handlers, only on terminal nodes
	seg    *segment
	pat    string
	nkids  int
	isWild bool // this node is a catch-all (matches regardless of trailing /)
	trail  bool // pattern ended with '/'
}

// leaf holds the handlers of a terminal node. Keeping it out of tnode shrinks
// every internal node (most nodes), which matters for cache behaviour under
// concurrent load.
type leaf struct {
	h     [numMethods]methodEntry
	other map[string]methodEntry // custom methods
	all   methodEntry            // handler for every method (Mount/Any)
	bits  uint16                 // methods with a handler, for Allow
	allow string                 // precomputed Allow header value
}

type staticChild struct {
	key string
	n   *tnode
}

// addStatic returns the static child for key, creating it if needed.
func (n *tnode) addStatic(key string) *tnode {
	switch n.nkids {
	case 0:
		c := &tnode{}
		n.one = staticChild{key, c}
		n.nkids = 1
		return c
	case 1:
		if n.one.key == key {
			return n.one.n
		}
		c := &tnode{}
		n.kids = []staticChild{n.one, {key, c}}
		n.one = staticChild{}
		n.nkids = 2
		n.sortKids()
		return c
	}
	for i := range n.kids {
		if n.kids[i].key == key {
			return n.kids[i].n
		}
	}
	c := &tnode{}
	n.kids = append(n.kids, staticChild{key, c})
	n.nkids++
	n.sortKids()
	return c
}

func (n *tnode) sortKids() {
	slices.SortFunc(n.kids, func(a, b staticChild) int { return strings.Compare(a.key, b.key) })
	if n.nkids > firstByteThreshold {
		n.rebuildFirst()
	}
}

// firstByteThreshold is where the bucket index starts paying for its memory.
const firstByteThreshold = 8

// smallBucket is the bucket size below which a direct compare beats extracting
// the segment and binary-searching it.
const smallBucket = 4

// rebuildFirst rebuilds the first-byte bucket index (registration-time).
func (n *tnode) rebuildFirst() {
	if n.first == nil {
		n.first = make([]int16, 257)
	}
	i := 0
	for b := 0; b < 256; b++ {
		for i < len(n.kids) && int(n.kids[i].key[0]) < b {
			i++
		}
		n.first[b] = int16(i)
	}
	n.first[256] = int16(len(n.kids))
}

// childAt matches a static child directly against path[start:], returning the
// child and the index just past the segment. It compares against the path with
// no segment extraction and no hashing.
func (n *tnode) childAt(path string, start int) (*tnode, int) {
	switch n.nkids {
	case 0:
		return nil, 0
	case 1:
		if c, adv, ok := matchChild(n.one, path, start); ok {
			return c, adv
		}
		return nil, 0
	}
	if n.first != nil && start < len(path) {
		b := int(path[start])
		lo, ub := int(n.first[b]), int(n.first[b+1])
		if ub-lo <= smallBucket {
			for i := lo; i < ub; i++ {
				if c, adv, ok := matchChild(n.kids[i], path, start); ok {
					return c, adv
				}
			}
			return nil, 0
		}
		if lo < ub {
			j := nextSlash(path, start)
			key := path[start:j]
			for lo < ub {
				mid := int(uint(lo+ub) >> 1)
				if n.kids[mid].key < key {
					lo = mid + 1
				} else {
					ub = mid
				}
			}
			if lo < len(n.kids) && n.kids[lo].key == key {
				return n.kids[lo].n, j
			}
		}
		return nil, 0
	}
	for i := range n.kids {
		if c, adv, ok := matchChild(n.kids[i], path, start); ok {
			return c, adv
		}
	}
	return nil, 0
}

func matchChild(c staticChild, path string, start int) (*tnode, int, bool) {
	key := c.key
	if len(path)-start >= len(key) && path[start:start+len(key)] == key {
		adv := start + len(key)
		if adv == len(path) || path[adv] == '/' {
			return c.n, adv, true
		}
	}
	return nil, 0, false
}

// addParam returns the param/mixed edge for sg, creating it if needed. Edges
// are kept constrained-first so a specific {id:[0-9]+} is tried before a bare
// {id}, matching Chi's node order.
func (n *tnode) addParam(sg *segment) *tnode {
	for _, c := range n.params {
		if c.seg != nil && c.seg.expr == sg.expr && c.seg.kind == sg.kind {
			return c
		}
	}
	c := &tnode{seg: sg}
	if sg.v != nil {
		n.params = append([]*tnode{c}, n.params...)
	} else {
		n.params = append(n.params, c)
	}
	return c
}

// methodEntry is a handler plus its route's param names: the same shape can
// use different names per method (e.g. GET /u/{id} and POST /u/{name}).
type methodEntry struct {
	h    TypedHandler
	keys *[8]string
	mw   http.Handler // non-nil when the route has inline middlewares
}

func (n *tnode) set(method string, h TypedHandler, keys *[8]string, mw http.Handler) {
	l := n.leaf
	if l == nil {
		l = &leaf{}
		n.leaf = l
	}
	if method == "" {
		l.all = methodEntry{h, keys, mw}
		l.bits = 1<<numMethods - 1
		l.allow = buildAllow(l.bits, nil)
		return
	}
	if i := methodIndex(method); i >= 0 {
		l.h[i] = methodEntry{h, keys, mw}
		l.bits |= 1 << i
	} else {
		if l.other == nil {
			l.other = map[string]methodEntry{}
		}
		l.other[method] = methodEntry{h, keys, mw}
	}
	names := make([]string, 0, len(l.other))
	for m := range l.other {
		names = append(names, m)
	}
	l.allow = buildAllow(l.bits, names)
}

// buildAllow precomputes the sorted, comma-joined Allow value (net/http's
// format), so a 405 costs no allocation. Registration-time only.
func buildAllow(bits uint16, other []string) string {
	ms := make([]string, 0, numMethods+len(other))
	for i := range numMethods {
		if bits&(1<<i) != 0 {
			ms = append(ms, methodNames[i])
		}
	}
	ms = append(ms, other...)
	if bits&1 != 0 && bits&(1<<5) == 0 {
		ms = append(ms, http.MethodHead) // HEAD from GET
	}
	slices.Sort(ms)
	return strings.Join(ms, ", ")
}

func (n *tnode) entry(method string) (methodEntry, bool) {
	l := n.leaf
	if l == nil {
		return methodEntry{}, false
	}
	if i := methodIndex(method); i >= 0 {
		if l.h[i].h != nil {
			return l.h[i], true
		}
		if method == http.MethodHead && l.h[0].h != nil { // HEAD from GET
			return l.h[0], true
		}
	} else if e, ok := l.other[method]; ok {
		return e, true
	}
	if l.all.h != nil { // Mount/Any: any method not otherwise handled
		return l.all, true
	}
	return methodEntry{}, false
}

// Mux is a segment-trie router with typed handlers.
type Mux struct {
	trie   *tnode
	root   *Mux
	prefix string

	inline []Middleware // With/Group/Route-level middlewares

	// root-only state
	use              []Middleware
	chain            http.Handler
	frozen           bool
	ctx              bool // a Context is needed (Use/inline/mount present)
	mounted          bool // this mux is mounted under another one
	notFound         http.Handler
	methodNotAllowed http.Handler
}

// NewMux creates an empty Mux.
func NewMux() *Mux {
	m := &Mux{trie: &tnode{}}
	m.root = m
	return m
}

// Use appends middlewares, like chi. On the root mux it must be called before
// routes; on a Group/Route sub-mux it scopes to that sub-mux.
func (t *Mux) Use(mws ...Middleware) {
	if t.root == t {
		if t.frozen {
			panic("router: all middlewares must be defined before routes on a mux")
		}
		t.use = append(t.use, mws...)
	} else {
		t.inline = append(t.inline, mws...)
	}
	if len(mws) > 0 {
		t.root.ctx = true
	}
}

// With returns a sub-mux with extra inline middlewares, like chi.
func (t *Mux) With(mws ...Middleware) *Mux {
	t.root.frozen = true
	if len(mws) > 0 {
		t.root.ctx = true
	}
	return &Mux{
		trie:             t.trie,
		root:             t.root,
		prefix:           t.prefix,
		inline:           append(slices.Clone(t.inline), mws...),
		notFound:         t.root.notFound,
		methodNotAllowed: t.root.methodNotAllowed,
	}
}

// Group scopes middlewares without a path prefix, like chi.
func (t *Mux) Group(fn func(*Mux)) *Mux {
	im := t.With()
	if fn != nil {
		fn(im)
	}
	return im
}

// Route scopes a path prefix, like chi.
func (t *Mux) Route(pattern string, fn func(*Mux)) *Mux {
	if fn == nil {
		panic("router: attempting to Route() a nil subrouter on '" + pattern + "'")
	}
	t.root.frozen = true
	sub := &Mux{
		trie:             t.trie,
		root:             t.root,
		prefix:           joinPrefix(t.prefix, pattern),
		inline:           slices.Clone(t.inline),
		notFound:         t.root.notFound,
		methodNotAllowed: t.root.methodNotAllowed,
	}
	fn(sub)
	return sub
}

// Mount attaches another handler at pattern, like chi. The handler sees the
// path with the mount point stripped; params accumulate across the mount.
func (t *Mux) Mount(pattern string, h http.Handler) {
	if h == nil {
		panic("router: attempting to Mount() a nil handler on '" + pattern + "'")
	}
	t.root.frozen = true
	t.root.ctx = true
	if sub, ok := h.(*Mux); ok {
		sub.root.mounted = true // its requests may carry a parent Context
	}
	serve := func(w http.ResponseWriter, r *http.Request, rest string) {
		if rest == "" {
			rest = "/"
		} else {
			rest = "/" + rest
		}
		r2 := new(http.Request)
		*r2 = *r
		u2 := new(url.URL)
		*u2 = *r.URL
		u2.Path = rest
		u2.RawPath = ""
		r2.URL = u2
		h.ServeHTTP(w, r2)
	}
	sub := func(w http.ResponseWriter, r *http.Request, ps Params) { serve(w, r, ps.Get("*")) }
	if pattern == "/" || pattern == "" {
		t.HandleAll("/*", sub)
		return
	}
	t.HandleAll(pattern, func(w http.ResponseWriter, r *http.Request, _ Params) { serve(w, r, "") })
	t.HandleAll(joinPrefix(pattern, "/*"), sub)
}

// NotFound sets the 404 handler.
func (t *Mux) NotFound(h http.HandlerFunc) { t.root.notFound = h }

// MethodNotAllowed sets the 405 handler.
func (t *Mux) MethodNotAllowed(h http.HandlerFunc) { t.root.methodNotAllowed = h }

// Handle registers a typed handler for method/pattern.
func (t *Mux) Handle(method, pattern string, h TypedHandler) {
	t.register(strings.ToUpper(method), pattern, h)
}

// HandleAll registers a typed handler for every method (used by Mount).
func (t *Mux) HandleAll(pattern string, h TypedHandler) {
	t.register("", pattern, h)
}

func (t *Mux) register(method, pattern string, h TypedHandler) {
	if pattern == "" || pattern[0] != '/' {
		panic("router: pattern must begin with '/' in '" + pattern + "'")
	}
	t.root.freeze()
	full := joinPrefix(t.prefix, pattern)
	trailing := strings.HasSuffix(full, "/") && full != "/"
	segs := mustParse(full)
	n := t.trie
	for i := range segs {
		sg := segs[i]
		switch sg.kind {
		case segStatic:
			n = n.addStatic(sg.text)
		case segParam, segMixed:
			n = n.addParam(&sg)
		case segWild:
			if n.wild == nil {
				n.wild = &tnode{seg: &sg, isWild: true}
			}
			n = n.wild
		}
	}
	n.trail = trailing
	n.pat = full
	keys := routeKeys(segs)
	var mw http.Handler
	if len(t.inline) > 0 {
		inner := h
		mw = chainMiddlewares(t.inline, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inner(w, r, RouteContext(r.Context()).params)
		}))
	}
	n.set(method, h, &keys, mw)
}

// freeze builds the root middleware chain once (middleware constructors run a
// single time) and marks the mux as registered.
func (t *Mux) freeze() {
	t.frozen = true
	if t.chain == nil {
		t.chain = chainMiddlewares(t.use, http.HandlerFunc(t.dispatchCtx))
	}
}

func joinPrefix(prefix, pattern string) string {
	if prefix == "" {
		return pattern
	}
	if !strings.HasPrefix(pattern, "/") {
		pattern = "/" + pattern
	}
	if pattern == "/" {
		return strings.TrimSuffix(prefix, "/") + "/"
	}
	return strings.TrimSuffix(prefix, "/") + pattern
}

// Get registers a typed GET handler.
func (t *Mux) Get(pattern string, h TypedHandler) { t.Handle(http.MethodGet, pattern, h) }

// Post registers a typed POST handler.
func (t *Mux) Post(pattern string, h TypedHandler) { t.Handle(http.MethodPost, pattern, h) }

// Put registers a typed PUT handler.
func (t *Mux) Put(pattern string, h TypedHandler) { t.Handle(http.MethodPut, pattern, h) }

// HandleFunc registers a compatible http.HandlerFunc. Parameters are published
// on the request (URLParam/req.PathValue work), costing one pooled Context.
func (t *Mux) HandleFunc(method, pattern string, fn http.HandlerFunc) {
	t.Handle(method, pattern, func(w http.ResponseWriter, r *http.Request, ps Params) {
		rctx := ctxPool.Get().(*Context)
		rctx.Reset()
		defer ctxPool.Put(rctx)
		for i := 0; i < ps.n; i++ {
			k, v := ps.At(i)
			rctx.params.Add(k, v)
			r.SetPathValue(k, v)
		}
		fn(w, r.WithContext(context.WithValue(r.Context(), RouteCtxKey, rctx)))
	})
}

// GetFunc registers a compatible GET handler.
func (t *Mux) GetFunc(pattern string, fn http.HandlerFunc) {
	t.HandleFunc(http.MethodGet, pattern, fn)
}

// trieMatch is the per-request matching state: captured params plus the
// methods seen on nodes that matched the path under another method (for 405).
type trieMatch struct {
	ps        Params
	entry     methodEntry // resolved by find
	bits      uint16
	other     []string
	allowNode *tnode // first node that matched the path under another method
	multi     bool   // more than one such node (needs the union)
}

func (m *trieMatch) allow(n *tnode) {
	if n.leaf == nil {
		return
	}
	m.bits |= n.leaf.bits
	for k := range n.leaf.other {
		if !slices.Contains(m.other, k) {
			m.other = append(m.other, k)
		}
	}
	if m.allowNode == nil {
		m.allowNode = n
	} else if m.allowNode != n {
		m.multi = true
	}
}

// nextSlash returns the index of the next '/' at or after i (or len(path)).
// Long remainders use strings.IndexByte, which is SIMD-accelerated in the
// runtime; short ones use a byte loop to avoid the call overhead.
func nextSlash(path string, i int) int {
	if len(path)-i >= 16 {
		if k := strings.IndexByte(path[i:], '/'); k >= 0 {
			return i + k
		}
		return len(path)
	}
	for i < len(path) && path[i] != '/' {
		i++
	}
	return i
}

// ServeHTTP matches the request against the trie and dispatches. Without
// middlewares or mounts this is the allocation-free fast path; with them it
// runs the middleware chain over the request-scoped Context, like Compat.
func (t *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	root := t.root
	if root != t {
		root.ServeHTTP(w, r)
		return
	}
	if !root.ctx && !root.mounted {
		root.dispatch(w, r)
		return
	}
	rctx := RouteContext(r.Context())
	if rctx == nil {
		rctx = ctxPool.Get().(*Context)
		rctx.Reset()
		defer ctxPool.Put(rctx)
		r = r.WithContext(context.WithValue(r.Context(), RouteCtxKey, rctx))
	}
	if root.chain == nil {
		root.dispatchCtx(w, r)
		return
	}
	root.chain.ServeHTTP(w, r)
}

// dispatch is the fast path: no Context, params passed by value.
func (t *Mux) dispatch(w http.ResponseWriter, r *http.Request) {
	path := requestPath(r)
	var m trieMatch
	n := t.find(t.trie, path, 0, r.Method, len(path) > 1 && path[len(path)-1] == '/', &m)
	if n == nil {
		t.miss(w, r, &m)
		return
	}
	r.Pattern = n.pat
	m.ps.keys = m.entry.keys
	m.entry.h(w, r, m.ps)
}

// dispatchCtx is the middleware/mount path: params go into the Context, and
// the handler (or its inline chain) reads them from there.
func (t *Mux) dispatchCtx(w http.ResponseWriter, r *http.Request) {
	rctx := RouteContext(r.Context())
	path := requestPath(r)
	var m trieMatch
	n := t.find(t.trie, path, 0, r.Method, len(path) > 1 && path[len(path)-1] == '/', &m)
	if n == nil {
		t.miss(w, r, &m)
		return
	}
	r.Pattern = n.pat
	m.ps.keys = m.entry.keys
	for i := 0; i < m.ps.n; i++ {
		k, v := m.ps.At(i)
		rctx.params.Add(k, v)
	}
	if m.entry.mw != nil {
		m.entry.mw.ServeHTTP(w, r)
		return
	}
	m.entry.h(w, r, rctx.params)
}

// miss answers 405 when the path exists under another method, else 404.
func (t *Mux) miss(w http.ResponseWriter, r *http.Request, m *trieMatch) {
	if m.bits != 0 || len(m.other) > 0 {
		t.notAllowed(w, r, m)
		return
	}
	t.notFoundOr(w, r)
}

func requestPath(r *http.Request) string {
	path := r.URL.RawPath
	if path == "" {
		path = r.URL.Path
	}
	if path == "" {
		path = "/"
	}
	return path
}

// find walks the trie, returning the first node that matches the path AND has
// a handler for method. Static edges are deterministic, so they are walked in a
// loop; recursion (with param rollback) is only used to backtrack over
// param/mixed/wildcard edges. Nodes that match the path but not the method are
// accumulated into m (=> 405).
func (t *Mux) find(n *tnode, path string, i int, method string, trail bool, m *trieMatch) *tnode {
	var seg string
	var j int
	for {
		if i == len(path) {
			return t.terminal(n, method, trail, m)
		}
		if n.nkids != 0 {
			if c, adv := n.childAt(path, i+1); c != nil {
				n, i = c, adv
				continue
			}
		}
		j = nextSlash(path, i+1)
		seg = path[i+1 : j]
		if seg == "" && j == len(path) {
			// The path ends here (root, or a trailing slash): a catch-all
			// takes the empty remainder, otherwise this is the terminal node.
			if n.wild != nil {
				mark := m.ps.n
				m.ps.add(path[i+1:])
				if f := t.find(n.wild, path, len(path), method, trail, m); f != nil {
					return f
				}
				m.ps.n = mark
			}
			i = len(path)
			continue
		}
		break
	}
	if seg != "" {
		for _, pn := range n.params {
			mark := m.ps.n
			sg := pn.seg
			if sg.kind == segParam && sg.v == nil {
				m.ps.add(seg) // common case: no validation, no call
			} else if !matchParam(sg, seg, &m.ps) {
				continue
			}
			if f := t.find(pn, path, j, method, trail, m); f != nil {
				return f
			}
			m.ps.n = mark // backtrack
		}
	}
	if n.wild != nil {
		mark := m.ps.n
		m.ps.add(path[i+1:])
		if f := t.find(n.wild, path, len(path), method, trail, m); f != nil {
			return f
		}
		m.ps.n = mark
	}
	return nil
}

// terminal checks a node at the end of the path: trailing-slash match (a
// catch-all ignores it), method, and 405 bookkeeping.
func (t *Mux) terminal(n *tnode, method string, trail bool, m *trieMatch) *tnode {
	if !n.isWild && n.trail != trail {
		return nil
	}
	if e, ok := n.entry(method); ok {
		m.entry = e
		return n
	}
	if n.leaf != nil && (n.leaf.bits != 0 || len(n.leaf.other) > 0) {
		m.allow(n)
	}
	return nil
}

// matchParam validates seg against the param/mixed edge and appends its params.
func matchParam(sg *segment, seg string, ps *Params) bool {
	if sg.kind == segParam {
		if sg.v != nil && !sg.v(seg) {
			return false
		}
		ps.add(seg)
		return true
	}
	vals, ok := splitMixed(sg.parts, seg)
	if !ok {
		return false
	}
	for _, kv := range vals {
		ps.add(kv[1])
	}
	return true
}

// routeKeys lists the param names of a pattern in match order.
func routeKeys(segs []segment) [8]string {
	var keys [8]string
	n := 0
	for i := range segs {
		sg := &segs[i]
		switch sg.kind {
		case segParam:
			keys[n] = sg.name
			n++
		case segMixed:
			for _, p := range sg.parts {
				if p.isParam {
					keys[n] = p.name
					n++
				}
			}
		case segWild:
			keys[n] = "*"
			n++
		}
		if n == len(keys) {
			break
		}
	}
	return keys
}

// notFoundOr serves the 404 handler, or a bare 404. Unlike Compat (which keeps
// net/http's body), Mux is its own engine and defaults to an empty 404 body:
// zero allocations. Set NotFound for a custom body.
func (t *Mux) notFoundOr(w http.ResponseWriter, r *http.Request) {
	if t.notFound != nil {
		t.notFound.ServeHTTP(w, r)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// notAllowed serves 405 with the stdlib Allow format, or the custom handler.
func (t *Mux) notAllowed(w http.ResponseWriter, r *http.Request, m *trieMatch) {
	if t.methodNotAllowed != nil {
		t.methodNotAllowed.ServeHTTP(w, r)
		return
	}
	if !m.multi && m.allowNode != nil && m.allowNode.leaf != nil {
		w.Header().Set("Allow", m.allowNode.leaf.allow)
	} else {
		w.Header().Set("Allow", buildAllow(m.bits, m.other))
	}
	// Bare 405 (no body), like the bare 404: Mux is its own engine.
	w.WriteHeader(http.StatusMethodNotAllowed)
}
