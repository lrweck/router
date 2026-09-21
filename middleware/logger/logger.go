// Package logger logs one line per request with log/slog.
package logger

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/lrweck/router/middleware/internal/wrap"
	"github.com/lrweck/router/middleware/requestid"
)

// New returns a middleware that logs method, path, matched route pattern,
// status, response bytes and duration for every request. A nil logger uses
// slog.Default.
//
// The request ID is logged when the requestid middleware is in the chain (put
// it before this one to read it from the context, though the response header is
// checked too).
func New(log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := wrap.New(w)
			next.ServeHTTP(ww, r)

			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"route", r.Pattern,
				"status", ww.Status(),
				"bytes", ww.Bytes(),
				"duration", time.Since(start),
			}
			if id := requestid.From(r); id != "" {
				attrs = append(attrs, "request_id", id)
			} else if id := ww.Header().Get(requestid.Header); id != "" {
				attrs = append(attrs, "request_id", id)
			}
			log.Log(r.Context(), slog.LevelInfo, "request", attrs...)
		})
	}
}
