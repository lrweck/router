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
// It builds typed slog.Attr values and calls LogAttrs, so the numeric fields
// (status, bytes, duration) are not boxed into any.
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

			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("route", r.Pattern),
				slog.Int("status", ww.Status()),
				slog.Int64("bytes", ww.Bytes()),
				slog.Duration("duration", time.Since(start)),
			}
			if id := requestID(r, ww); id != "" {
				attrs = append(attrs, slog.String("request_id", id))
			}
			log.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
		})
	}
}

// requestID prefers the context (requestid outside this middleware) and falls
// back to the response header (requestid inside it).
func requestID(r *http.Request, w http.ResponseWriter) string {
	if id := requestid.From(r); id != "" {
		return id
	}
	return w.Header().Get(requestid.Header)
}
