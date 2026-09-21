package timeout

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDeadlineOnContext(t *testing.T) {
	var deadline time.Time
	var has bool
	h := New(50 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, has = r.Context().Deadline()
	}))
	start := time.Now()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !has {
		t.Fatal("no deadline on the request context")
	}
	if d := deadline.Sub(start); d < 40*time.Millisecond || d > 60*time.Millisecond {
		t.Errorf("deadline in %v, want ~50ms", d)
	}
}

func TestDeadlineExceeded(t *testing.T) {
	var err error
	h := New(10 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		err = r.Context().Err()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if err != context.DeadlineExceeded {
		t.Errorf("ctx.Err() = %v, want DeadlineExceeded", err)
	}
}
