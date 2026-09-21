package wrap

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStatusAndBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	w := New(rec)
	if w.Status() != 200 || w.Wrote() {
		t.Fatalf("before write: status=%d wrote=%v", w.Status(), w.Wrote())
	}
	w.WriteHeader(201)
	w.Write([]byte("hello"))
	if w.Status() != 201 || w.Bytes() != 5 || !w.Wrote() {
		t.Errorf("status=%d bytes=%d wrote=%v", w.Status(), w.Bytes(), w.Wrote())
	}
	if rec.Code != 201 || rec.Body.String() != "hello" {
		t.Errorf("recorder = %d %q", rec.Code, rec.Body.String())
	}
}

func TestWriteDefaultsTo200(t *testing.T) {
	rec := httptest.NewRecorder()
	w := New(rec)
	w.Write([]byte("x"))
	if w.Status() != 200 {
		t.Errorf("status = %d, want 200", w.Status())
	}
}

func TestSecondWriteHeaderIgnored(t *testing.T) {
	w := New(httptest.NewRecorder())
	w.WriteHeader(200)
	w.WriteHeader(500)
	if w.Status() != 200 {
		t.Errorf("status = %d, want 200", w.Status())
	}
}

func TestReadFromCounts(t *testing.T) {
	rec := httptest.NewRecorder()
	w := New(rec)
	n, err := w.ReadFrom(strings.NewReader("abcdef"))
	if err != nil || n != 6 || w.Bytes() != 6 {
		t.Fatalf("ReadFrom = %d, %v; bytes = %d", n, err, w.Bytes())
	}
}

func TestUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	if New(rec).Unwrap() != rec {
		t.Error("Unwrap did not return the wrapped writer")
	}
}

func TestHijackUnsupported(t *testing.T) {
	if _, _, err := New(httptest.NewRecorder()).Hijack(); err == nil {
		t.Error("Hijack on a non-hijackable writer should fail")
	}
}
