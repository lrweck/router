// Package cors answers cross-origin requests (CORS).
//
// CORS is defined by the WHATWG Fetch standard, not an RFC; the Origin header
// it relies on is RFC 6454. This middleware follows the Fetch algorithm: it
// echoes the origin (or "*"), never combines "*" with credentials, and sets
// Vary so shared caches keep origins apart.
package cors

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Options configures the middleware.
type Options struct {
	// AllowedOrigins lists the exact origins allowed, or "*" for any.
	AllowedOrigins []string
	// AllowedMethods defaults to GET, HEAD, POST, PUT, PATCH, DELETE.
	AllowedMethods []string
	// AllowedHeaders defaults to echoing Access-Control-Request-Headers.
	AllowedHeaders []string
	// ExposedHeaders lists the response headers the browser may read.
	ExposedHeaders []string
	// AllowCredentials sets Access-Control-Allow-Credentials.
	AllowCredentials bool
	// MaxAge is the preflight cache lifetime; zero omits Access-Control-Max-Age.
	MaxAge time.Duration
}

// New returns a middleware that sets the CORS response headers and answers
// preflight requests with 204. Requests without an Origin, or from a
// disallowed origin, pass through untouched (still with Vary: Origin).
func New(o Options) func(http.Handler) http.Handler {
	allowAll := false
	origins := make(map[string]struct{}, len(o.AllowedOrigins))
	for _, or := range o.AllowedOrigins {
		if or == "*" {
			allowAll = true
			continue
		}
		origins[or] = struct{}{}
	}
	methods := o.AllowedMethods
	if len(methods) == 0 {
		methods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	}
	methodList := strings.Join(methods, ", ")
	headerList := strings.Join(o.AllowedHeaders, ", ")
	exposed := strings.Join(o.ExposedHeaders, ", ")
	maxAge := ""
	if o.MaxAge > 0 {
		maxAge = strconv.Itoa(int(o.MaxAge / time.Second))
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Add("Vary", "Origin") // even on a miss, so caches do not mix origins

			allowed := allowAll
			if !allowed {
				_, allowed = origins[origin]
			}
			if !allowed {
				next.ServeHTTP(w, r)
				return
			}

			// "*" is only valid without credentials; otherwise echo the origin.
			if allowAll && !o.AllowCredentials {
				h.Set("Access-Control-Allow-Origin", "*")
			} else {
				h.Set("Access-Control-Allow-Origin", origin)
			}
			if o.AllowCredentials {
				h.Set("Access-Control-Allow-Credentials", "true")
			}
			if exposed != "" {
				h.Set("Access-Control-Expose-Headers", exposed)
			}

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				// A preflight response varies on the requested method/headers
				// too, or a shared cache could replay the wrong answer.
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				h.Set("Access-Control-Allow-Methods", methodList)
				switch {
				case headerList != "":
					h.Set("Access-Control-Allow-Headers", headerList)
				default:
					if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
						h.Set("Access-Control-Allow-Headers", req)
					}
				}
				if maxAge != "" {
					h.Set("Access-Control-Max-Age", maxAge)
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
