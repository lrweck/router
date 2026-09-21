// Package recoverer turns a handler panic into a 500 and a log line, instead of
// crashing the connection.
package recoverer

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// New returns a middleware that recovers panics, logs them with a stack trace,
// and answers 500 when nothing has been written yet. http.ErrAbortHandler is
// re-panicked so net/http can abort the connection as it intends.
//
// A nil logger uses slog.Default.
func New(log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				log.ErrorContext(r.Context(), "panic recovered",
					"err", rec,
					"method", r.Method,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				if ww, ok := w.(interface{ Wrote() bool }); ok && ww.Wrote() {
					return // headers already sent; nothing left to do
				}
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}()
			next.ServeHTTP(w, r)
		})
	}
}
