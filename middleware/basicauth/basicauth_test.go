package basicauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUsers(t *testing.T) {
	h := New(Config{Users: map[string]string{"alice": "s3cret"}})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, c := range []struct {
		user, pass string
		want       int
	}{
		{"alice", "s3cret", http.StatusNoContent},
		{"alice", "wrong", http.StatusUnauthorized},
		{"bob", "s3cret", http.StatusUnauthorized},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.SetBasicAuth(c.user, c.pass)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s/%s = %d, want %d", c.user, c.pass, rec.Code, c.want)
		}
	}
}

func TestChallenge(t *testing.T) {
	h := New(Config{Realm: "My Realm"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="My Realm"` {
		t.Errorf("challenge = %q", got)
	}
}

func TestValidator(t *testing.T) {
	h := New(Config{Validator: func(u, p string) bool { return u == "x" && p == "y" }})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	req := httptest.NewRequest("GET", "/", nil)
	req.SetBasicAuth("x", "y")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestRealmSanitized(t *testing.T) {
	h := New(Config{Realm: `a"b\c`})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="abc"` {
		t.Errorf("challenge = %q", got)
	}
}
