// Chi-compatible routing context.
//
// URLParam reads from this context first (params accumulate across mounted
// sub-routers, exactly like Chi) and falls back to the stdlib PathValue.
package router

import (
	"context"
	"net/http"
	"strings"
	"sync"
)

// Context is the routing context of a request, like chi.Context (trimmed to
// what a stdlib-backed router needs: params and matched patterns).
type Context struct {
	params        Params
	keybuf        [8]string
	RoutePatterns []string
}

// Reset clears the context for reuse, like chi.
func (x *Context) Reset() {
	x.params = Params{keys: &x.keybuf}
	x.RoutePatterns = x.RoutePatterns[:0]
}

// URLParam returns the parameter value (newest first, so mounted sub-routers
// shadow parent params with the same key), like chi.
func (x *Context) URLParam(key string) string {
	return x.params.Get(key)
}

// RoutePattern builds the routing pattern of the request across sub-routers,
// like chi.
func (x *Context) RoutePattern() string {
	if x == nil {
		return ""
	}
	p := strings.Join(x.RoutePatterns, "")
	for strings.Contains(p, "/*/") {
		p = strings.ReplaceAll(p, "/*/", "/")
	}
	if p != "/" {
		p = strings.TrimSuffix(p, "//")
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

type contextKey struct {
	name string
}

func (k *contextKey) String() string { return "router context value " + k.name }

// RouteCtxKey is the request-context key of the routing context, like chi.
var RouteCtxKey = &contextKey{"RouteContext"}

// RouteContext returns the routing context of a request, like chi.
func RouteContext(ctx context.Context) *Context {
	val, _ := ctx.Value(RouteCtxKey).(*Context)
	return val
}

// NewRouteContext returns a new routing context, like chi.
func NewRouteContext() *Context {
	c := &Context{}
	c.Reset()
	return c
}

// URLParamFromCtx returns the URL parameter from a request context, like chi.
func URLParamFromCtx(ctx context.Context, key string) string {
	if rctx := RouteContext(ctx); rctx != nil {
		return rctx.URLParam(key)
	}
	return ""
}

// Param returns the URL parameter of a request. It is the primary accessor
// (shorter than URLParam, which is kept as an alias for Chi compatibility).
func Param(r *http.Request, key string) string {
	return URLParam(r, key)
}

// ParamsOf returns all matched path parameters of a request, or the zero value
// outside a routed request.
func ParamsOf(r *http.Request) Params {
	if rctx := RouteContext(r.Context()); rctx != nil {
		return rctx.params
	}
	var p Params
	return p
}

// URLParam returns the URL parameter of a request, like chi.URLParam.
// Reads the routing context (it spans mounted sub-routers and holds the user's
// param names); outside a routed request it falls back to r.PathValue.
func URLParam(r *http.Request, key string) string {
	if rctx := RouteContext(r.Context()); rctx != nil {
		if v := rctx.params.Get(key); v != "" {
			return v
		}
		if key == "rest" { // "rest" is an alias for the wildcard value
			return rctx.params.Get("*")
		}
		return ""
	}
	return r.PathValue(key)
}

var ctxPool = sync.Pool{New: func() any { return NewRouteContext() }}

// Params are the matched path parameters of a typed handler. Passed by value
// (no allocation); the names point at the route's shared key array, so only
// the values are copied. Up to 8 parameters are captured.
type Params struct {
	keys *[8]string
	vals [8]string
	n    int
}

// Len returns the number of parameters.
func (p Params) Len() int { return p.n }

// Get returns the value of the named parameter (newest first, so mounted
// sub-routers shadow parent params with the same key).
func (p Params) Get(name string) string {
	if p.keys == nil {
		return ""
	}
	for i := p.n - 1; i >= 0; i-- {
		if p.keys[i] == name {
			return p.vals[i]
		}
	}
	return ""
}

// ByName is an alias for Get.
func (p Params) ByName(name string) string { return p.Get(name) }

// At returns the i-th parameter name and value.
func (p Params) At(i int) (name, value string) {
	if p.keys == nil || i < 0 || i >= p.n {
		return "", ""
	}
	return p.keys[i], p.vals[i]
}

// Add appends a parameter (keys must be set, as the Context does).
func (p *Params) Add(name, value string) {
	if p.n < len(p.vals) && p.keys != nil {
		p.keys[p.n], p.vals[p.n] = name, value
		p.n++
	}
}

// clearWild blanks the "*"/"rest" values when entering a mount, like Chi.
func (p *Params) clearWild() {
	if p.keys == nil {
		return
	}
	for i := p.n - 1; i >= 0; i-- {
		if k := p.keys[i]; k == "*" || k == "rest" {
			p.vals[i] = ""
		}
	}
}

func (p *Params) add(value string) {
	if p.n < len(p.vals) {
		p.vals[p.n] = value
		p.n++
	}
}
