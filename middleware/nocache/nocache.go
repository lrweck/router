// Package nocache marks responses as uncacheable.
package nocache

import "net/http"

// New returns a middleware that sets the headers browsers and proxies read to
// keep a response out of any cache.
func New() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Cache-Control", "no-cache, no-store, no-transform, must-revalidate, private, max-age=0")
			h.Set("Expires", "Thu, 01 Jan 1970 00:00:00 GMT")
			h.Set("Pragma", "no-cache")
			h.Set("X-Accel-Expires", "0")
			next.ServeHTTP(w, r)
		})
	}
}
