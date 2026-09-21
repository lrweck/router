// Chi-style pattern parsing and per-route matching state.
//
// A Chi pattern is compiled once at registration into segments, then desugared
// to one or two stdlib ServeMux patterns (placeholders, never user names, so
// any Chi param name works). The same segments drive matchPath, used for
// 405 detection and Find, with identical validator semantics as the serve path.
package router

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

type segKind int

const (
	segStatic segKind = iota
	segParam
	segMixed
	segWild
)

// segPart is one piece of a mixed segment: literal text or a param.
type segPart struct {
	static  string
	name    string
	expr    string // raw constraint source, for the structural signature
	isParam bool
	v       validator
}

// segment is one '/'-separated piece of a Chi pattern.
type segment struct {
	kind   segKind
	text   string // static
	name   string // param ("" = anonymous, validate only)
	expr   string // raw constraint source (param/mixed)
	v      validator
	parts  []segPart // mixed
	ph     string    // stdlib placeholder for this segment
	direct bool      // ph == name: the mux sets it natively, no re-publish
	glued  bool      // wildcard glued to the static prefix ("/api*"), no slash
}

// route is one registered route (or mount point).
type route struct {
	method   string // "" = all methods
	pattern  string // full Chi pattern
	segs     []segment
	trailing bool // pattern ends with "/" (matches with and without it)

	mountBase string // mount point base path
	isMount   bool
	sub       Routes

	endpoint http.Handler
	inline   []Middleware
	owner    *Compat
	score    int

	// simple is set when extraction can neither fail nor rename anything: no
	// validators, no mixed segments, no wildcard, all params native. The mux
	// already set their PathValue, so serving skips extract entirely.
	simple bool

	// needsCtx is set when serving this route requires the request-scoped
	// routing Context: mounts (param accumulation across sub-routers), mixed
	// segments, or params renamed to a stdlib placeholder. Plain routes with
	// native placeholder names read params from r.PathValue directly and pay
	// no context allocation.
	needsCtx bool
}

// matchResult is the outcome of matchPath.
type matchResult struct {
	params    [][2]string
	remainder string // mount remainder, always starting with "/"
}

func mustParse(full string) []segment {
	if full == "" || full[0] != '/' {
		panic(fmt.Sprintf("router: pattern %q must begin with '/'", full))
	}
	if full == "/" {
		return nil
	}
	if i := strings.IndexByte(full, '*'); i >= 0 {
		if i != len(full)-1 {
			panic(fmt.Sprintf("router: invalid pattern %q: wildcard '*' must be the last value in a route", full))
		}
		base := full[:i]
		if base == "" {
			return []segment{{kind: segWild}}
		}
		// "prefix*" (no slash) also matches the bare prefix, like Chi.
		glued := !strings.HasSuffix(base, "/")
		return append(mustParse(base), segment{kind: segWild, glued: glued})
	}
	raw := strings.Split(full[1:], "/")
	if raw[len(raw)-1] == "" {
		raw = raw[:len(raw)-1] // trailing "/" lives on route.trailing
	}
	segs := make([]segment, 0, len(raw))
	for i, s := range raw {
		segs = append(segs, parseSeg(s, i == len(raw)-1, full))
	}
	seen := map[string]bool{}
	for _, sg := range segs {
		switch sg.kind {
		case segParam:
			dupCheck(seen, sg.name, full)
		case segMixed:
			for _, p := range sg.parts {
				if p.isParam {
					dupCheck(seen, p.name, full)
				}
			}
		}
	}
	return segs
}

func dupCheck(seen map[string]bool, name, full string) {
	if seen[name] {
		panic(fmt.Sprintf("router: pattern %q contains duplicate param key %q", full, name))
	}
	seen[name] = true
}

func parseSeg(s string, last bool, full string) segment {
	fail := func(format string, args ...any) {
		panic(fmt.Sprintf("router: invalid pattern %q: %s", full, fmt.Sprintf(format, args...)))
	}
	if strings.HasPrefix(s, "*") {
		if !last {
			fail("wildcard '*' must be the last value in a route")
		}
		return segment{kind: segWild}
	}
	if strings.Contains(s, "*") {
		fail("wildcard '*' must be the last value in a route")
	}
	if !strings.Contains(s, "{") {
		if strings.Contains(s, "}") {
			fail("unbalanced '}'")
		}
		return segment{kind: segStatic, text: s}
	}
	var parts []segPart
	var static strings.Builder
	flush := func() {
		if static.Len() > 0 {
			parts = append(parts, segPart{static: static.String()})
			static.Reset()
		}
	}
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '}' {
			fail("unbalanced '}'")
		}
		if c != '{' {
			static.WriteByte(c)
			i++
			continue
		}
		depth := 0
		j := i
		for j < len(s) {
			if s[j] == '{' {
				depth++
			} else if s[j] == '}' {
				depth--
				if depth == 0 {
					break
				}
			}
			j++
		}
		if j >= len(s) {
			fail("missing '}'")
		}
		name, expr, _ := strings.Cut(s[i+1:j], ":")
		flush()
		parts = append(parts, segPart{name: name, expr: expr, isParam: true, v: compileConstraint(expr, full)})
		i = j + 1
	}
	flush()
	for k := 0; k+1 < len(parts); k++ {
		if parts[k].isParam && parts[k+1].isParam {
			fail("adjacent params need a separator")
		}
	}
	if len(parts) == 1 && parts[0].isParam {
		return segment{kind: segParam, name: parts[0].name, expr: parts[0].expr, v: parts[0].v}
	}
	return segment{kind: segMixed, parts: parts}
}

func scoreSegs(segs []segment) int {
	score := 0
	for _, sg := range segs {
		switch sg.kind {
		case segStatic:
			score += 5
		case segParam:
			// Constrained (regexp) params are tried before mixed/plain
			// segments, matching Chi's node order (regexp before param).
			if sg.v != nil {
				score += 4
			} else {
				score += 2
			}
		case segMixed:
			score += 3
		case segWild:
			score -= 8
		}
	}
	return score
}

// stdNameOK reports whether a param name can be used verbatim as a stdlib
// wildcard name (so the mux sets PathValue natively, no re-publish needed).
func stdNameOK(s string) bool {
	if s == "" || s == "rest" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || i > 0 && '0' <= c && c <= '9' {
			continue
		}
		return false
	}
	return true
}

// shapeKey identifies a route's stdlib pattern shape, ignoring param names and
// constraints: two routes with the same key desugar to the same pattern.
func shapeKey(segs []segment, trailing bool) string {
	var b strings.Builder
	for _, sg := range segs {
		b.WriteByte('/')
		switch sg.kind {
		case segStatic:
			b.WriteString(sg.text)
		case segParam, segMixed:
			b.WriteByte('#')
		case segWild:
			b.WriteString("#...")
		}
	}
	if trailing {
		b.WriteString("/$")
	}
	return b.String()
}

// classify sets the per-route flags used to pick the serving path: simple
// (extraction can be skipped entirely) and needsCtx (the request-scoped
// Context is required).
func (rt *route) classify() {
	rt.simple = !rt.isMount
	for i := range rt.segs {
		switch sg := &rt.segs[i]; sg.kind {
		case segStatic:
		case segParam:
			if !sg.direct {
				rt.needsCtx = true
			}
			if !sg.direct || sg.v != nil {
				rt.simple = false
			}
		default: // mixed, wild
			rt.needsCtx = true
			rt.simple = false
		}
	}
}

// assignNames applies the shape registry to the route's param segments and
// names the trailing wildcard "rest" (native PathValue, free of collisions).
func (rt *route) assignNames(root *Compat) {
	base := rt.segs
	if n := len(rt.segs); n > 0 && rt.segs[n-1].kind == segWild {
		base = rt.segs[:n-1]
	}
	root.applyShapeNames(base, rt.trailing)
	for i := range rt.segs {
		if rt.segs[i].kind == segWild {
			rt.segs[i].ph = "rest"
			rt.segs[i].direct = true
		}
	}
}

// applyShapeNames assigns stdlib placeholder names to segs in place, sharing
// them per pattern shape. The first route at a shape keeps the user's names
// when stdlib-safe, so the mux sets PathValue natively (no per-request map on
// re-publish); later routes at the same shape reuse them so patterns agree.
func (r *Compat) applyShapeNames(segs []segment, trailing bool) {
	key := shapeKey(segs, trailing)
	names := r.root.shapeNames[key]
	if names == nil {
		names = make([]string, len(segs))
		used := map[string]bool{}
		for i := range segs {
			sg := &segs[i]
			switch sg.kind {
			case segParam:
				if stdNameOK(sg.name) && !used[sg.name] {
					names[i] = sg.name
				} else {
					names[i] = fmt.Sprintf("s%d", i+1)
				}
			case segMixed:
				names[i] = fmt.Sprintf("s%d", i+1)
			}
			used[names[i]] = true
		}
		r.root.shapeNames[key] = names
	}
	for i := range segs {
		if names[i] == "" {
			continue
		}
		segs[i].ph = names[i]
		segs[i].direct = segs[i].kind == segParam && names[i] == segs[i].name && segs[i].name != ""
	}
}

// signature identifies a route's structure and constraints, ignoring param
// names: routes with equal signatures are the same route and the last
// registration wins (Chi overwrites); routes whose signatures differ at the
// same slot are fallbacks tried in specificity order.
func (rt *route) signature() string {
	var b strings.Builder
	for _, sg := range rt.segs {
		b.WriteByte('/')
		switch sg.kind {
		case segStatic:
			b.WriteString(sg.text)
		case segParam:
			b.WriteString("{")
			b.WriteString(sg.expr)
			b.WriteString("}")
		case segMixed:
			for _, p := range sg.parts {
				if p.isParam {
					b.WriteByte('{')
					b.WriteString(p.expr)
					b.WriteByte('}')
				} else {
					b.WriteString(p.static)
				}
			}
		case segWild:
			b.WriteString("/*")
		}
	}
	if rt.trailing {
		b.WriteByte('/')
	}
	return b.String()
}

// canonicalPath renders segments as a stdlib ServeMux pattern using their
// canonical placeholder names.
func canonicalPath(segs []segment) string {
	var b strings.Builder
	for _, sg := range segs {
		b.WriteByte('/')
		switch sg.kind {
		case segStatic:
			b.WriteString(sg.text)
		case segParam, segMixed:
			b.WriteByte('{')
			b.WriteString(sg.ph)
			b.WriteByte('}')
		case segWild:
			b.WriteByte('{')
			b.WriteString(sg.ph)
			b.WriteString("...}")
		}
	}
	return b.String()
}

// stdPatterns desugars the Chi pattern to stdlib ServeMux patterns.
func (rt *route) stdPatterns(method string) []string {
	p := canonicalPath(rt.segs)
	if p == "" {
		// Root "/": exact match only, never the catch-all subtree.
		if method == "" {
			return []string{"/{$}"}
		}
		return []string{method + " /{$}"}
	}
	if n := len(rt.segs); n > 0 && rt.segs[n-1].kind == segWild && rt.segs[n-1].glued {
		// "prefix*" matches both the bare prefix and its subtree.
		bare := canonicalPath(rt.segs[:n-1])
		if method == "" {
			return []string{p, bare}
		}
		return []string{method + " " + p, method + " " + bare}
	}
	if rt.trailing {
		// A pattern written with a trailing slash matches only that exact
		// path (like Chi's trailing node); the bare spelling is covered by
		// Mount()'s stub, not here.
		if method == "" {
			return []string{p + "/{$}"}
		}
		return []string{method + " " + p + "/{$}"}
	}
	if method == "" {
		return []string{p}
	}
	return []string{method + " " + p}
}

// inner wraps the endpoint in the route's inline middlewares.
func (rt *route) inner() http.Handler {
	h := rt.endpoint
	for _, v := range slices.Backward(rt.inline) {
		h = v(h)
	}
	return h
}

// try extracts and validates the request's placeholders; on success it
// publishes params/pattern and serves, reporting true. This is the per-slot
// fallback unit: several constrained routes can share one stdlib pattern.
func (rt *route) try(w http.ResponseWriter, req *http.Request, inner http.Handler) bool {
	if rt.simple {
		// Native params: the mux already set PathValue. Publish into the
		// routing Context only when one exists (i.e. inside a mount).
		req.Pattern = rt.pattern
		if rctx := RouteContext(req.Context()); rctx != nil {
			for i := range rt.segs {
				if sg := &rt.segs[i]; sg.kind == segParam {
					rctx.params.Add(sg.name, req.PathValue(sg.ph))
				}
			}
			rctx.RoutePatterns = append(rctx.RoutePatterns, rt.pattern)
		}
		inner.ServeHTTP(w, req)
		return true
	}
	// Buffer lives in try's frame (extract writes into it and returns a
	// count), so it stays on the stack: returning the slice made it escape.
	var buf [8][2]string
	n, wild, ok := rt.extract(req, buf[:0])
	if !ok {
		return false
	}
	vals := buf[:n]
	for _, kv := range vals {
		if kv[0] != "" && !rt.directParam(kv[0]) {
			req.SetPathValue(kv[0], kv[1])
		}
	}
	if wild.set {
		// Keep stdlib-style r.PathValue("*"); "rest" is native when the
		// wildcard placeholder kept that name.
		req.SetPathValue("*", wild.v)
	}
	req.Pattern = rt.pattern

	rctx := RouteContext(req.Context())
	if rctx == nil && rt.needsCtx {
		rctx = ctxPool.Get().(*Context)
		rctx.Reset()
		req = req.WithContext(context.WithValue(req.Context(), RouteCtxKey, rctx))
		defer ctxPool.Put(rctx)
	}
	if rctx != nil {
		for _, kv := range vals {
			rctx.params.Add(kv[0], kv[1])
		}
		if wild.set {
			rctx.params.Add("*", wild.v)
		}
		rctx.RoutePatterns = append(rctx.RoutePatterns, rt.pattern)
	}
	inner.ServeHTTP(w, req)
	return true
}

// directParam reports whether the mux already set this param natively (the
// placeholder kept the user's name), so re-publishing it would only allocate.
func (rt *route) directParam(name string) bool {
	for i := range rt.segs {
		sg := &rt.segs[i]
		if sg.direct && sg.name == name {
			return true
		}
	}
	return false
}

type wildVal struct {
	v   string
	set bool
}

// extract reads placeholder values set by the mux, splits mixed segments and
// runs validators, appending values to dst (caller-owned, so it can stay on
// the stack) and returning how many were written. Same semantics as matchPath.
func (rt *route) extract(req *http.Request, dst [][2]string) (int, wildVal, bool) {
	vals := dst[:0]
	var wv wildVal
	for i := range rt.segs {
		sg := &rt.segs[i]
		switch sg.kind {
		case segStatic:
			continue
		case segParam:
			v := req.PathValue(sg.ph)
			if v == "" {
				return 0, wv, false
			}
			if sg.v != nil && !sg.v(v) {
				return 0, wv, false
			}
			vals = append(vals, [2]string{sg.name, v})
		case segMixed:
			raw := req.PathValue(sg.ph)
			if raw == "" {
				return 0, wv, false
			}
			var ok bool
			vals, ok = appendMixed(vals, sg.parts, raw)
			if !ok {
				return 0, wv, false
			}
		case segWild:
			wv = wildVal{req.PathValue(sg.ph), true}
		}
	}
	return len(vals), wv, true
}

// appendMixed splits a mixed-segment value on its static parts, appending
// into out (caller-owned, usually stack-backed, so no escape).
func appendMixed(out [][2]string, parts []segPart, raw string) ([][2]string, bool) {
	pos := 0
	for i, p := range parts {
		if !p.isParam {
			if !strings.HasPrefix(raw[pos:], p.static) {
				return nil, false
			}
			pos += len(p.static)
			continue
		}
		next := ""
		for j := i + 1; j < len(parts); j++ {
			if !parts[j].isParam {
				next = parts[j].static
				break
			}
		}
		var v string
		if next == "" {
			v = raw[pos:]
			pos = len(raw)
		} else {
			idx := strings.Index(raw[pos:], next)
			if idx < 0 {
				return nil, false
			}
			v = raw[pos : pos+idx]
			pos += idx
		}
		if p.v != nil && !p.v(v) {
			return nil, false
		}
		out = append(out, [2]string{p.name, v})
	}
	if pos != len(raw) {
		return nil, false
	}
	return out, true
}

// splitMixed splits a mixed-segment value on its static parts.
func splitMixed(parts []segPart, raw string) ([][2]string, bool) {
	return appendMixed(nil, parts, raw)
}

// matchShape reports whether path matches, without collecting params and
// without allocating (Cut-based segment walk, no Split). Used on the miss
// path (404/405), where per-route garbage would amplify junk traffic.
// Same accept/reject semantics as matchPath.
func (rt *route) matchShape(path string) (remainder string, ok bool) {
	// Mirror matchPath's trailing rule without allocating: an exact-length
	// route only matches a trailing-slash path if the route itself trails
	// (wildcard routes absorb the slash either way).
	hadTrailing := len(path) > 1 && path[len(path)-1] == '/'
	rest := strings.TrimPrefix(path, "/")
	for _, sg := range rt.segs {
		if sg.kind == segWild {
			return "/" + rest, true
		}
		var cur string
		cur, rest, _ = strings.Cut(rest, "/")
		switch sg.kind {
		case segStatic:
			if cur != sg.text {
				return "", false
			}
		case segParam:
			if cur == "" {
				return "", false
			}
			if sg.v != nil && !sg.v(cur) {
				return "", false
			}
		case segMixed:
			if !checkMixed(sg.parts, cur) {
				return "", false
			}
		}
	}
	if rest != "" {
		return "", false
	}
	if hadTrailing && !rt.trailing {
		return "", false
	}
	return "", true
}

// checkMixed is splitMixed without value collection.
func checkMixed(parts []segPart, raw string) bool {
	pos := 0
	for i, p := range parts {
		if !p.isParam {
			if !strings.HasPrefix(raw[pos:], p.static) {
				return false
			}
			pos += len(p.static)
			continue
		}
		next := ""
		for j := i + 1; j < len(parts); j++ {
			if !parts[j].isParam {
				next = parts[j].static
				break
			}
		}
		var v string
		if next == "" {
			v = raw[pos:]
			pos = len(raw)
		} else {
			idx := strings.Index(raw[pos:], next)
			if idx < 0 {
				return false
			}
			v = raw[pos : pos+idx]
			pos += idx
		}
		if p.v != nil && !p.v(v) {
			return false
		}
	}
	return pos == len(raw)
}

// matchPath matches a path without serving: shapes plus validators.
// Used for Find (which needs the values).
func (rt *route) matchPath(path string) (matchResult, bool) {
	var res matchResult
	s := strings.TrimPrefix(path, "/")
	var pseg []string
	if s != "" {
		pseg = strings.Split(s, "/")
	}
	if rt.trailing && len(pseg) > 0 && pseg[len(pseg)-1] == "" {
		pseg = pseg[:len(pseg)-1]
	}
	params, rest, ok := matchSegs(rt.segs, pseg)
	if !ok {
		return res, false
	}
	res.params = params
	if rt.isMount {
		res.remainder = "/" + strings.Join(rest, "/")
	}
	return res, true
}

func matchSegs(segs []segment, pseg []string) (params [][2]string, rest []string, ok bool) {
	var out [][2]string
	i := 0
	for _, sg := range segs {
		if sg.kind == segWild {
			r := strings.Join(pseg[i:], "/")
			out = append(out, [2]string{"*", r})
			return out, pseg[i:], true
		}
		if i >= len(pseg) {
			return nil, nil, false
		}
		cur := pseg[i]
		switch sg.kind {
		case segStatic:
			if cur != sg.text {
				return nil, nil, false
			}
		case segParam:
			if cur == "" {
				return nil, nil, false
			}
			if sg.v != nil && !sg.v(cur) {
				return nil, nil, false
			}
			out = append(out, [2]string{sg.name, cur})
		case segMixed:
			vs, ok := splitMixed(sg.parts, cur)
			if !ok {
				return nil, nil, false
			}
			out = append(out, vs...)
		}
		i++
	}
	if i != len(pseg) {
		return nil, nil, false
	}
	return out, nil, true
}
