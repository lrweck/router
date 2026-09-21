// Package timeout puts a deadline on the request context.
package timeout

import (
	"context"
	"net/http"
	"time"
)

// New returns a middleware that gives every request a context deadline of d.
// The handler and everything downstream must honor the context; the middleware
// itself does not cut off a handler that ignores it (that is what
// http.TimeoutHandler does, with its own caveats).
func New(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
