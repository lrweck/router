// Package wrap provides the ResponseWriter wrapper shared by the middleware
// packages. It records the status code and the number of body bytes written and
// forwards the optional interfaces (Flusher, Hijacker, ReaderFrom, Pusher) to
// the underlying writer, so wrapping does not hide them from the handler.
package wrap

import (
	"bufio"
	"io"
	"net"
	"net/http"
)

// Writer wraps an http.ResponseWriter, recording the status and byte count.
type Writer struct {
	http.ResponseWriter
	status int
	n      int64
}

// New returns a Writer wrapping w.
func New(w http.ResponseWriter) *Writer { return &Writer{ResponseWriter: w} }

func (w *Writer) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *Writer) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.n += int64(n)
	return n, err
}

// Status returns the status code, defaulting to 200 before anything is written.
func (w *Writer) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// Bytes returns the number of body bytes written.
func (w *Writer) Bytes() int64 { return w.n }

// Wrote reports whether a status or body was written.
func (w *Writer) Wrote() bool { return w.status != 0 }

// Unwrap exposes the underlying writer to http.ResponseController.
func (w *Writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush forwards to the underlying writer if it can flush.
func (w *Writer) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Hijack forwards to the underlying writer if it can be hijacked.
func (w *Writer) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// ReadFrom takes the underlying writer's fast path when it has one, counting
// the bytes either way.
func (w *Writer) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		w.n += n
		return n, err
	}
	n, err := io.Copy(w.ResponseWriter, r)
	w.n += n
	return n, err
}

// Push forwards to the underlying writer if it supports server push.
func (w *Writer) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}
