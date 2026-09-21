// Package throttle bounds how many requests run at once.
package throttle

import "net/http"

// New returns a middleware that allows at most limit requests to run at once.
// Requests beyond the limit wait for a slot, or answer 503 if their context is
// canceled first.
func New(limit int) func(http.Handler) http.Handler {
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-r.Context().Done():
				http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
