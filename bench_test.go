package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mustValidator(expr string) validator {
	v := compileConstraint(expr, "bench")
	if v == nil {
		panic("nil validator for " + expr)
	}
	return v
}

var benchInputs = map[string]string{
	"digits15":  "123456789012345",
	"digits128": strings.Repeat("1234567890", 12) + "12345678",
	"slug120":   strings.Repeat("abc-def-0123456789", 6),
	"date":      "2026",
}

func BenchmarkBytesInClass(b *testing.B) {
	classes := []struct {
		name   string
		ranges [][2]byte
		chars  []byte
		input  string
	}{
		{"digits/short", [][2]byte{{'0', '9'}}, nil, "123456789012345"},
		{"digits/long", [][2]byte{{'0', '9'}}, nil, strings.Repeat("1234567890", 12) + "12345678"},
		{"slug/long", [][2]byte{{'a', 'z'}, {'0', '9'}}, []byte{'-'}, strings.Repeat("abc-def-0123456789", 6)},
		{"upper/short", [][2]byte{{'A', 'Z'}}, nil, "ABCDEFGHIJKL"},
		// Tail-heavy lengths (len % 32 == 31) to expose the SIMD tail path.
		{"digits/tail31", [][2]byte{{'0', '9'}}, nil, strings.Repeat("7", 127)},
		{"slug/tail31", [][2]byte{{'a', 'z'}, {'0', '9'}}, []byte{'-'}, strings.Repeat("ab-", 42) + "a"},
	}
	for _, c := range classes {
		in := []byte(c.input)
		b.Run("build/"+c.name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if !bytesInClass(in, c.ranges, c.chars) {
					b.Fatal("mismatch")
				}
			}
		})
		b.Run("loop/"+c.name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if !bytesInClassLoop(in, c.ranges, c.chars) {
					b.Fatal("mismatch")
				}
			}
		})
		b.Run("swar/"+c.name, func(b *testing.B) {
			b.SetBytes(int64(len(in)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if !swarAllInClass(in, c.ranges, c.chars) {
					b.Fatal("mismatch")
				}
			}
		})
	}
}

func BenchmarkValidator(b *testing.B) {
	cases := []struct{ name, expr, input string }{
		{"digits+/short", `[0-9]+`, benchInputs["digits15"]},
		{"digits+/long", `[0-9]+`, benchInputs["digits128"]},
		{"slug+/long", `[a-z0-9-]+`, benchInputs["slug120"]},
		{"date4", `\d{4}`, benchInputs["date"]},
		{"dot+/long", `.+`, benchInputs["slug120"]},
	}
	for _, c := range cases {
		v := mustValidator(c.expr)
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.input)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if !v(c.input) {
					b.Fatal("mismatch")
				}
			}
		})
	}
}

func BenchmarkDispatchConstrained(b *testing.B) {
	r := NewRouter()
	r.Get("/users/{id:[0-9]+}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/users/123456789012345", nil)
	b.ReportAllocs()

	for b.Loop() {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req.Clone(b.Context()))
		if w.Code != http.StatusNoContent {
			b.Fatalf("code=%d", w.Code)
		}
	}
}

func BenchmarkDispatchMixed(b *testing.B) {
	r := NewRouter()
	r.Get("/d/{month:[0-9]+}-{day:[0-9]+}", func(w http.ResponseWriter, req *http.Request) {
		if URLParam(req, "month") == "" || URLParam(req, "day") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/d/09-21", nil)
	b.ReportAllocs()

	for b.Loop() {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req.Clone(b.Context()))
		if w.Code != http.StatusNoContent {
			b.Fatalf("code=%d", w.Code)
		}
	}
}

func BenchmarkDispatchNotFound(b *testing.B) {
	r := NewRouter()
	r.Get("/users/{id:[0-9]+}", func(w http.ResponseWriter, _ *http.Request) {})
	r.Post("/users/{id:[0-9]+}", func(w http.ResponseWriter, _ *http.Request) {})
	r.Get("/articles/{slug:[a-z-]+}", func(w http.ResponseWriter, _ *http.Request) {})
	req := httptest.NewRequest(http.MethodGet, "/nope/nothing/here", nil)
	b.ReportAllocs()

	for b.Loop() {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req.Clone(b.Context()))
		if w.Code != http.StatusNotFound {
			b.Fatalf("code=%d", w.Code)
		}
	}
}

func BenchmarkValidatorGeneric(b *testing.B) {
	v := mustValidator(`(ab|cd)+`)
	in := strings.Repeat("ab", 40)
	b.SetBytes(int64(len(in)))
	b.ReportAllocs()
	for b.Loop() {
		if !v(in) {
			b.Fatal("mismatch")
		}
	}
}

// benchServe runs h once per iteration with a fresh recorder and a cloned
// request (the recorder and clone are harness cost shared by every case,
// including the raw ServeMux baseline).
func benchServe(b *testing.B, h http.Handler, method, path string, want int) {
	b.Helper()
	req := httptest.NewRequest(method, path, nil)
	b.ReportAllocs()
	for b.Loop() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req.Clone(b.Context()))
		if w.Code != want {
			b.Fatalf("code=%d want=%d", w.Code, want)
		}
	}
}

// BenchmarkDispatchStatic is the common case: one plain param route, no
// constraint (the slot has a single always-matching candidate).
func BenchmarkDispatchStatic(b *testing.B) {
	r := NewRouter()
	r.Get("/users/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	benchServe(b, r, http.MethodGet, "/users/42", http.StatusNoContent)
}

// BenchmarkServeMuxStatic is the raw stdlib baseline for the same shape.
func BenchmarkServeMuxStatic(b *testing.B) {
	mux := http.NewServeMux()
	mux.Handle("GET /users/{id}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	benchServe(b, mux, http.MethodGet, "/users/42", http.StatusNoContent)
}

// BenchmarkServeMuxPlain is the raw stdlib floor for a static route.
func BenchmarkServeMuxPlain(b *testing.B) {
	mux := http.NewServeMux()
	mux.Handle("GET /ping", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	benchServe(b, mux, http.MethodGet, "/ping", http.StatusNoContent)
}

func BenchmarkDispatchPlain(b *testing.B) {
	r := NewRouter()
	r.Get("/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	benchServe(b, r, http.MethodGet, "/ping", http.StatusNoContent)
}

// BenchmarkDispatchMiddleware adds a 3-deep Use stack to the static case.
func BenchmarkDispatchMiddleware(b *testing.B) {
	r := NewRouter()
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { next.ServeHTTP(w, req) })
	}
	r.Use(mw, mw, mw)
	r.Get("/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	benchServe(b, r, http.MethodGet, "/ping", http.StatusNoContent)
}
