package throttle

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestLimitsConcurrency(t *testing.T) {
	const limit = 3
	var mu sync.Mutex
	cur, max := 0, 0
	h := New(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cur++
		if cur > max {
			max = cur
		}
		mu.Unlock()
		for i := 0; i < 10000; i++ { // hold the slot for a moment
		}
		mu.Lock()
		cur--
		mu.Unlock()
	}))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		}()
	}
	wg.Wait()
	if max > limit {
		t.Errorf("max concurrency = %d, want <= %d", max, limit)
	}
}

func TestCanceledContextAnswers503(t *testing.T) {
	hold, holdCancel := context.WithCancel(context.Background())
	inside := make(chan struct{})
	h := New(1)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inside)
		<-hold.Done()
	}))
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil).WithContext(hold))
	}()
	<-inside // the single slot is now taken

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil).WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rec.Code)
	}
	holdCancel()
}
