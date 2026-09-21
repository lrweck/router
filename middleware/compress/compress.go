// Package compress gzips responses when the client accepts it.
package compress

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// New returns a middleware that gzips the response when Accept-Encoding allows
// it. level is passed to gzip.NewWriterLevel (gzip.DefaultCompression is a good
// default; an invalid level falls back to it).
//
// Responses with no body (1xx, 204, 304) and responses that already carry a
// Content-Encoding are passed through untouched.
func New(level int) func(http.Handler) http.Handler {
	pool := &sync.Pool{New: func() any {
		zw, err := gzip.NewWriterLevel(io.Discard, level)
		if err != nil {
			zw, _ = gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
		}
		return zw
	}}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
				next.ServeHTTP(w, r)
				return
			}
			gw := &writer{ResponseWriter: w, pool: pool}
			defer gw.close()
			next.ServeHTTP(gw, r)
		})
	}
}

// writer decides lazily, on the first Write/WriteHeader, whether the response
// gets compressed. That keeps no-body responses (204/304) and responses that
// set their own Content-Encoding out of the gzip path.
type writer struct {
	http.ResponseWriter
	pool *sync.Pool
	zw   *gzip.Writer
	done bool
}

func (w *writer) start(zip bool) {
	if w.done {
		return
	}
	w.done = true
	if !zip {
		return
	}
	h := w.Header()
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	h.Del("Content-Length")
	zw := w.pool.Get().(*gzip.Writer)
	zw.Reset(w.ResponseWriter)
	w.zw = zw
}

func (w *writer) WriteHeader(code int) {
	switch {
	case code < 200, code == http.StatusNoContent, code == http.StatusNotModified:
		w.start(false)
	default:
		w.start(w.Header().Get("Content-Encoding") == "")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *writer) Write(b []byte) (int, error) {
	if !w.done {
		w.start(w.Header().Get("Content-Encoding") == "")
	}
	if w.zw != nil {
		return w.zw.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *writer) close() {
	if w.zw != nil {
		_ = w.zw.Close()
		w.pool.Put(w.zw)
		w.zw = nil
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush pushes the gzip trailer's buffered data out before flushing the
// underlying writer.
func (w *writer) Flush() {
	if w.zw != nil {
		_ = w.zw.Flush()
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// acceptsGzip reports whether the request's Accept-Encoding allows gzip, per
// RFC 9110 §12.5.3. A missing header means "do not compress" here (the safe
// default for legacy clients); "*" is honored.
func acceptsGzip(ae string) bool {
	if ae == "" {
		return false
	}
	gzipQ, starQ := -1.0, -1.0
	for _, part := range strings.Split(ae, ",") {
		coding, params, _ := strings.Cut(part, ";")
		q := qvalue(params)
		switch {
		case strings.EqualFold(strings.TrimSpace(coding), "gzip"):
			gzipQ = q
		case strings.TrimSpace(coding) == "*":
			starQ = q
		}
	}
	if gzipQ >= 0 {
		return gzipQ > 0
	}
	return starQ > 0
}

// qvalue parses the ";q=" parameter of an Accept-Encoding entry (RFC 9110
// §12.4.2). It returns 1 when the parameter is absent or malformed.
func qvalue(params string) float64 {
	for _, p := range strings.Split(params, ";") {
		v, ok := strings.CutPrefix(strings.TrimSpace(p), "q=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if !validQ(v) {
			return 1
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 1
		}
		return f
	}
	return 1
}

// validQ matches RFC 9110's qvalue: "0"/"1" with up to three fractional digits.
func validQ(v string) bool {
	if len(v) == 0 || len(v) > 5 || (v[0] != '0' && v[0] != '1') {
		return false
	}
	if len(v) == 1 {
		return true
	}
	if v[1] != '.' {
		return false
	}
	for i := 2; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return false
		}
		if v[0] == '1' && v[i] != '0' {
			return false // a q of 1 allows only zero fractions
		}
	}
	return true
}
