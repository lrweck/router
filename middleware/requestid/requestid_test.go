package requestid

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func run(t *testing.T, incoming string) (ctxID, header string) {
	t.Helper()
	h := New()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxID = From(r)
	}))
	req := httptest.NewRequest("GET", "/", nil)
	if incoming != "" {
		req.Header.Set(Header, incoming)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return ctxID, rec.Header().Get(Header)
}

func TestReusesIncoming(t *testing.T) {
	ctxID, header := run(t, "abc-123")
	if ctxID != "abc-123" || header != "abc-123" {
		t.Errorf("ctx=%q header=%q", ctxID, header)
	}
}

func TestGeneratesWhenAbsent(t *testing.T) {
	ctxID, header := run(t, "")
	if ctxID == "" || header != ctxID {
		t.Errorf("ctx=%q header=%q", ctxID, header)
	}
}

func TestRejectsUnsafeIncoming(t *testing.T) {
	for _, bad := range []string{"has space", "a\nb", strings.Repeat("x", 65)} {
		ctxID, _ := run(t, bad)
		if ctxID == bad || ctxID == "" {
			t.Errorf("unsafe id %q was not replaced (got %q)", bad, ctxID)
		}
	}
}
