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
	// Static children. Up to 4 in an inline array (linear compare of short keys
	// beats hashing at low fan-out); a map beyond that. A segment-level radix
	// was measured slower here: keys are short, and a map hashes once while the
	// radix pays per-level pointer chasing.
	kids  [4]staticChild // inline children (low fan-out, no hashing)
	nkids int
	// Beyond 4 children: sorted slice + a first-byte bucket index (O(1) bucket,
	// then a binary search within it). Measured faster than a map for the short
	// segment keys a router sees.
	more  []staticChild
	first []int16 // 257 entries: first[b]..first[b+1] is the bucket for byte b

	params []*tnode // {name} / {a}-{b} edges, constrained first
	wild   *tnode   // * edge
	isWild bool     // this node is a catch-all (matches regardless of trailing /)
	seg    *segment
	trail  bool // pattern ended with '/'
	pat    string

	h     [numMethods]methodEntry
	other map[string]methodEntry // custom methods
	bits  uint16                 // methods with a handler, for Allow
	allow string                 // precomputed Allow header value
}

type staticChild struct {
	key string
	n   *tnode
}

// addStatic returns the static child for key, creating it if needed.
func (n *tnode) addStatic(key string) *tnode {
	for i := 0; i < n.nkids; i++ {
		if n.kids[i].key == key {
			return n.kids[i].n
		}
	}
	for i := range n.more {
		if n.more[i].key == key {
			return n.more[i].n
		}
	}
	if n.nkids < len(n.kids) && n.more == nil {
		c := &tnode{}
		n.kids[n.nkids] = staticChild{key, c}
		n.nkids++
		return c
	}
	if n.more == nil { // promote the inline children
		n.more = append(n.more, n.kids[:n.nkids]...)
		n.nkids = 0
	}
	c := &tnode{}
	n.more = append(n.more, staticChild{key, c})
	slices.SortFunc(n.more, func(a, b staticChild) int { return strings.Compare(a.key, b.key) })
	n.rebuildFirst()
	return c
}

// rebuildFirst rebuilds the first-byte bucket index (registration-time).
func (n *tnode) rebuildFirst() {
	if n.first == nil {
		n.first = make([]int16, 257)
	}
	i := 0
	for b := range 256 {
		for i < len(n.more) && int(n.more[i].key[0]) < b {
			i++
		}
		n.first[b] = int16(i)
	}
	n.first[256] = int16(len(n.more))
}

// childAt matches a static child directly against path[start:], returning the
// child and the index just past the segment. It compares against the path with
// no segment extraction and no hashing: inline children (low fan-out) linearly,
// the rest by a binary search on the first byte over a sorted slice.
func (n *tnode) childAt(path string, start int) (*tnode, int) {
	for k := 0; k < n.nkids; k++ {
		key := n.kids[k].key
		if len(path)-start >= len(key) && path[start:start+len(key)] == key {
			adv := start + len(key)
			if adv == len(path) || path[adv] == '/' {
				return n.kids[k].n, adv
			}
		}
	}
	if n.more != nil && start < len(path) {
		b := int(path[start])
		lo, hi := int(n.first[b]), int(n.first[b+1])
		if lo < hi {
			j := nextSlash(path, start)
			key := path[start:j]
			for lo < hi {
				mid := int(uint(lo+hi) >> 1)
				if n.more[mid].key < key {
					lo = mid + 1
				} else {
					hi = mid
				}
			}
			if lo < len(n.more) && n.more[lo].key == key {
				return n.more[lo].n, j
			}
		}
	}
	return nil, 0
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
}

func (n *tnode) set(method string, h TypedHandler, keys *[8]string) {
	if i := methodIndex(method); i >= 0 {
		n.h[i] = methodEntry{h, keys}
		n.bits |= 1 << i
	} else {
		if n.other == nil {
			n.other = map[string]methodEntry{}
		}
		n.other[method] = methodEntry{h, keys}
	}
	n.allow = buildAllow(n.bits, n.other)
}

// buildAllow precomputes the sorted, comma-joined Allow value (net/http's
// format), so a 405 costs no allocation. Registration-time only.
func buildAllow(bits uint16, other map[string]methodEntry) string {
	ms := make([]string, 0, numMethods)
	for i := range numMethods {
		if bits&(1<<i) != 0 {
			ms = append(ms, methodNames[i])
		}
	}
	for m := range other {
		ms = append(ms, m)
	}
	if bits&1 != 0 && bits&(1<<5) == 0 {
		ms = append(ms, http.MethodHead) // HEAD from GET
	}
	slices.Sort(ms)
	return strings.Join(ms, ", ")
}

func (n *tnode) entry(method string) (methodEntry, bool) {
	if i := methodIndex(method); i >= 0 {
		if n.h[i].h != nil {
			return n.h[i], true
		}
		if method == http.MethodHead && n.h[0].h != nil { // HEAD from GET
			return n.h[0], true
		}
		return methodEntry{}, false
	}
	e, ok := n.other[method]
	return e, ok
}

// Mux is a segment-trie router with typed handlers.
type Mux struct {
	root             *tnode
	notFound         http.Handler
	methodNotAllowed http.Handler
}

// NewMux creates an empty Mux.
func NewMux() *Mux { return &Mux{root: &tnode{}} }

// NotFound sets the 404 handler.
func (t *Mux) NotFound(h http.HandlerFunc) { t.notFound = h }

// MethodNotAllowed sets the 405 handler.
func (t *Mux) MethodNotAllowed(h http.HandlerFunc) { t.methodNotAllowed = h }

// Handle registers a typed handler for method/pattern.
func (t *Mux) Handle(method, pattern string, h TypedHandler) {
	if pattern == "" || pattern[0] != '/' {
		panic("router: pattern must begin with '/' in '" + pattern + "'")
	}
	trailing := strings.HasSuffix(pattern, "/") && pattern != "/"
	segs := mustParse(pattern)
	n := t.root
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
	n.pat = pattern
	keys := routeKeys(segs)
	n.set(strings.ToUpper(method), h, &keys)
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
	bits      uint16
	other     []string
	allowNode *tnode // first node that matched the path under another method
	multi     bool   // more than one such node (needs the union)
}

func (m *trieMatch) allow(n *tnode) {
	m.bits |= n.bits
	for k := range n.other {
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

// ServeHTTP matches the request against the trie and dispatches.
func (t *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.RawPath
	if path == "" {
		path = r.URL.Path
	}
	if path == "" {
		path = "/"
	}
	hadTrail := len(path) > 1 && path[len(path)-1] == '/'

	var m trieMatch
	n := t.find(t.root, path, 0, r.Method, hadTrail, &m)
	if n == nil {
		if m.bits != 0 || len(m.other) > 0 {
			t.notAllowed(w, r, &m)
		} else {
			t.notFoundOr(w, r)
		}
		return
	}
	e, _ := n.entry(r.Method)
	r.Pattern = n.pat
	m.ps.keys = e.keys
	e.h(w, r, m.ps)
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
		if n.nkids != 0 || n.more != nil {
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
	if _, ok := n.entry(method); ok {
		return n
	}
	if n.bits != 0 || len(n.other) > 0 {
		m.allow(n)
	}
	return nil
}

// matchParam validates seg against the param/mixed edge and appends its params.
func matchParam(sg *segment, seg string, ps *Params) bool {
	if sg == nil {
		return false
	}
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

// otherFrom adapts the custom-method list to the map buildAllow expects.
func otherFrom(names []string) map[string]methodEntry {
	if len(names) == 0 {
		return nil
	}
	m := make(map[string]methodEntry, len(names))
	for _, n := range names {
		m[n] = methodEntry{}
	}
	return m
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
	if !m.multi && m.allowNode != nil {
		w.Header().Set("Allow", m.allowNode.allow)
	} else {
		w.Header().Set("Allow", buildAllow(m.bits, otherFrom(m.other)))
	}
	// Bare 405 (no body), like the bare 404: Mux is its own engine.
	w.WriteHeader(http.StatusMethodNotAllowed)
}
