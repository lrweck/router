package logger

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lrweck/router/middleware/requestid"
)

func TestLogsRequest(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	// logger outside, requestid inside: the id is read from the response header.
	h := New(log)(requestid.New()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		w.Write([]byte("hi"))
	})))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))

	out := buf.String()
	for _, want := range []string{`"method":"GET"`, `"path":"/x"`, `"status":201`, `"bytes":2`, `"request_id"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %s", out, want)
		}
	}
}
