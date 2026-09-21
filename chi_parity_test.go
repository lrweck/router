// Behavioral parity tests ported from Chi's mux_test.go.
//
// Source: github.com/go-chi/chi (MIT, Copyright (c) 2015-present Peter
// Kieltyka, Google Inc.). These cases and their expectations are adapted from
// Chi's test suite; the handful of divergences are called out inline (empty
// path segments, escaped params, "//" cleaning) and are inherent to
// net/http.ServeMux. See NOTICE.md for the full attribution and license.
package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type ctxKey struct{ name string }

func testRequest(t *testing.T, h http.Handler, method, path string, body io.Reader) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, body).WithContext(t.Context())
	req.Host = "example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w, w.Body.String()
}

func mustBody(t *testing.T, h http.Handler, method, path, want string) {
	t.Helper()
	if _, body := testRequest(t, h, method, path, nil); body != want {
		t.Fatalf("%s %s: got %q, want %q", method, path, body, want)
	}
}

func TestChiMuxBasic(t *testing.T) {
	var count uint64
	countermw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count++
			next.ServeHTTP(w, r)
		})
	}
	usermw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), ctxKey{"user"}, "peter")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	exmw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), ctxKey{"ex"}, "a")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	logbuf := strings.Builder{}
	logmsg := "logmw test"
	logmw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logbuf.WriteString(logmsg)
			next.ServeHTTP(w, r)
		})
	}

	cxindex := func(w http.ResponseWriter, r *http.Request) {
		user := r.Context().Value(ctxKey{"user"}).(string)
		fmt.Fprintf(w, "hi %s", user)
	}
	ping := func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(".")) }
	headPing := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Ping", "1")
		w.WriteHeader(200)
	}
	createPing := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(201) }
	pingAll := func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ping all")) }
	pingAll2 := func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ping all2")) }
	pingOne := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ping one id: %s", URLParam(r, "id"))
	}
	pingWoop := func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("woop." + URLParam(r, "iidd")))
	}
	catchAll := func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("catchall")) }

	m := NewRouter()
	m.Use(countermw)
	m.Use(usermw)
	m.Use(exmw)
	m.Use(logmw)
	m.Get("/", cxindex)
	m.Method("GET", "/ping", http.HandlerFunc(ping))
	m.MethodFunc("GET", "/pingall", pingAll)
	m.MethodFunc("get", "/ping/all", pingAll)
	m.Get("/ping/all2", pingAll2)
	m.Head("/ping", headPing)
	m.Post("/ping", createPing)
	m.Get("/ping/{id}", pingWoop)
	m.Get("/ping/{id}", pingOne) // last registration wins
	m.Get("/ping/{iidd}/woop", pingWoop)
	m.HandleFunc("/admin/*", catchAll)

	if _, body := testRequest(t, m, "GET", "/", nil); body != "hi peter" {
		t.Fatal(body)
	}
	if logbuf.String() != logmsg {
		t.Error("expecting log message from middleware:", logmsg)
	}
	mustBody(t, m, "GET", "/ping", ".")
	mustBody(t, m, "GET", "/pingall", "ping all")
	mustBody(t, m, "GET", "/ping/all", "ping all")
	mustBody(t, m, "GET", "/ping/all2", "ping all2")
	mustBody(t, m, "GET", "/ping/123", "ping one id: 123")
	mustBody(t, m, "GET", "/ping/allan", "ping one id: allan")
	mustBody(t, m, "GET", "/ping/1/woop", "woop.1")
	mustBody(t, m, "HEAD", "/ping", "")
	mustBody(t, m, "GET", "/admin/catch-thazzzzz", "catchall")
	mustBody(t, m, "POST", "/admin/casdfsadfs", "catchall")

	// Custom http method on an unregistered route: 405.
	if resp, _ := testRequest(t, m, "DIE", "/ping/1/woop", nil); resp.Code != 405 {
		t.Fatalf("expecting 405, got %d", resp.Code)
	}
}

func TestChiMuxMounts(t *testing.T) {
	r := NewRouter()
	r.Get("/{hash}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "/%s", URLParam(r, "hash"))
	})
	r.Route("/{hash}/share", func(r Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "/%s/share", URLParam(r, "hash"))
		})
		r.Get("/{network}", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "/%s/share/%s", URLParam(r, "hash"), URLParam(r, "network"))
		})
	})
	m := NewRouter()
	m.Mount("/sharing", r)

	mustBody(t, m, "GET", "/sharing/aBc", "/aBc")
	mustBody(t, m, "GET", "/sharing/aBc/share", "/aBc/share")
	mustBody(t, m, "GET", "/sharing/aBc/share/twitter", "/aBc/share/twitter")
}

func TestChiMuxPlain(t *testing.T) {
	r := NewRouter()
	r.Get("/hi", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("bye")) })
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte("nothing here"))
	})
	mustBody(t, r, "GET", "/hi", "bye")
	mustBody(t, r, "GET", "/nothing-here", "nothing here")
}

func TestChiMuxEmptyRoutes(t *testing.T) {
	mux := NewRouter()
	apiRouter := NewRouter()
	mux.Handle("/api*", apiRouter)
	mustBody(t, mux, "GET", "/", "404 page not found\n")
	mustBody(t, apiRouter, "GET", "/", "404 page not found\n")
}

func TestChiMuxTrailingSlash(t *testing.T) {
	r := NewRouter()
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte("nothing here"))
	})
	subRoutes := NewRouter()
	indexHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(URLParam(r, "accountID")))
	}
	subRoutes.Get("/", indexHandler)
	r.Mount("/accounts/{accountID}", subRoutes)
	r.Get("/accounts/{accountID}/", indexHandler)

	mustBody(t, r, "GET", "/accounts/admin", "admin")
	mustBody(t, r, "GET", "/accounts/admin/", "admin")
	mustBody(t, r, "GET", "/nothing-here", "nothing here")
}

func TestChiMuxNestedNotFound(t *testing.T) {
	r := NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{"mw"}, "mw")))
		})
	})
	r.Get("/hi", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("bye")) })
	r.With(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{"with"}, "with")))
		})
	}).NotFound(func(w http.ResponseWriter, r *http.Request) {
		chkMw := r.Context().Value(ctxKey{"mw"}).(string)
		chkWith := r.Context().Value(ctxKey{"with"}).(string)
		w.WriteHeader(404)
		fmt.Fprintf(w, "root 404 %s %s", chkMw, chkWith)
	})

	sr1 := NewRouter()
	sr1.Get("/sub", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("sub")) })
	sr1.Group(func(sr1 Router) {
		sr1.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{"mw2"}, "mw2")))
			})
		})
		sr1.NotFound(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "sub 404 %s", r.Context().Value(ctxKey{"mw2"}).(string))
		})
	})

	sr2 := NewRouter()
	sr2.Get("/sub", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("sub2")) })

	r.Mount("/admin1", sr1)
	r.Mount("/admin2", sr2)

	mustBody(t, r, "GET", "/hi", "bye")
	mustBody(t, r, "GET", "/nothing-here", "root 404 mw with")
	mustBody(t, r, "GET", "/admin1/sub", "sub")
	mustBody(t, r, "GET", "/admin1/nope", "sub 404 mw2")
	mustBody(t, r, "GET", "/admin2/sub", "sub2")
	mustBody(t, r, "GET", "/admin2/nope", "root 404 mw with")
}

func TestChiMethodNotAllowed(t *testing.T) {
	r := NewRouter()
	r.Get("/hi", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("hi, get")) })
	r.Head("/hi", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("hi, head")) })
	r.Get("/*", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("catch-all")) })

	t.Run("Registered Method", func(t *testing.T) {
		resp, _ := testRequest(t, r, "GET", "/hi", nil)
		if resp.Code != 200 {
			t.Fatal(resp.Code)
		}
		if resp.Header().Values("Allow") != nil {
			t.Fatal("allow should be empty when method is registered")
		}
	})
	t.Run("Unregistered Method", func(t *testing.T) {
		resp, _ := testRequest(t, r, "POST", "/hi", nil)
		if resp.Code != 405 {
			t.Fatal(resp.Code)
		}
		// The default 405 comes from net/http, which emits a single
		// comma-joined Allow value (Chi emits one header per method).
		allowed := strings.Split(strings.Join(resp.Header().Values("Allow"), ","), ",")
		for i := range allowed {
			allowed[i] = strings.TrimSpace(allowed[i])
		}
		if len(allowed) != 2 ||
			((allowed[0] != "GET" || allowed[1] != "HEAD") && (allowed[1] != "GET" || allowed[0] != "HEAD")) {
			t.Fatal("Allow should contain GET and HEAD, got:", allowed)
		}
	})
}

func TestChiMuxNestedMethodNotAllowed(t *testing.T) {
	r := NewRouter()
	r.Get("/root", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("root")) })
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(405)
		w.Write([]byte("root 405"))
	})
	sr1 := NewRouter()
	sr1.Get("/sub1", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("sub1")) })
	sr1.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(405)
		w.Write([]byte("sub1 405"))
	})
	sr2 := NewRouter()
	sr2.Get("/sub2", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("sub2")) })
	pathVar := NewRouter()
	pathVar.Get("/{var}", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("pv")) })
	pathVar.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(405)
		w.Write([]byte("pv 405"))
	})
	r.Mount("/prefix1", sr1)
	r.Mount("/prefix2", sr2)
	r.Mount("/pathVar", pathVar)

	mustBody(t, r, "GET", "/root", "root")
	mustBody(t, r, "PUT", "/root", "root 405")
	mustBody(t, r, "GET", "/prefix1/sub1", "sub1")
	mustBody(t, r, "PUT", "/prefix1/sub1", "sub1 405")
	mustBody(t, r, "GET", "/prefix2/sub2", "sub2")
	mustBody(t, r, "PUT", "/prefix2/sub2", "root 405")
	mustBody(t, r, "GET", "/pathVar/myvar", "pv")
	mustBody(t, r, "DELETE", "/pathVar/myvar", "pv 405")
}

func TestChiMuxComplicatedNotFound(t *testing.T) {
	decorateRouter := func(r Router) {
		r.Get("/auth", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("auth get")) })
		r.Route("/public", func(r Router) {
			r.Get("/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("public get")) })
		})
		sub0 := NewRouter()
		sub0.Route("/resource", func(r Router) {
			r.Get("/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("private get")) })
		})
		r.Mount("/private", sub0)
		sub1 := NewRouter()
		sub1.Route("/resource", func(r Router) {
			r.Get("/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("private get")) })
		})
		r.With(func(next http.Handler) http.Handler { return next }).Mount("/private_mw", sub1)
	}
	testNotFound := func(t *testing.T, r Router) {
		mustBody(t, r, "GET", "/auth", "auth get")
		mustBody(t, r, "GET", "/public", "public get")
		mustBody(t, r, "GET", "/public/", "public get")
		mustBody(t, r, "GET", "/private/resource", "private get")
		mustBody(t, r, "GET", "/nope", "custom not-found")
		mustBody(t, r, "GET", "/public/nope", "custom not-found")
		mustBody(t, r, "GET", "/private/nope", "custom not-found")
		mustBody(t, r, "GET", "/private/resource/nope", "custom not-found")
		mustBody(t, r, "GET", "/private_mw/nope", "custom not-found")
		mustBody(t, r, "GET", "/private_mw/resource/nope", "custom not-found")
		mustBody(t, r, "GET", "/auth/", "custom not-found")
	}
	t.Run("pre", func(t *testing.T) {
		r := NewRouter()
		r.NotFound(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("custom not-found")) })
		decorateRouter(r)
		testNotFound(t, r)
	})
	t.Run("post", func(t *testing.T) {
		r := NewRouter()
		decorateRouter(r)
		r.NotFound(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("custom not-found")) })
		testNotFound(t, r)
	})
}

func TestChiMuxWith(t *testing.T) {
	var cmwInit1, cmwHandler1, cmwInit2, cmwHandler2 uint64
	mw1 := func(next http.Handler) http.Handler {
		cmwInit1++
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cmwHandler1++
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{"inline1"}, "yes")))
		})
	}
	mw2 := func(next http.Handler) http.Handler {
		cmwInit2++
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cmwHandler2++
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{"inline2"}, "yes")))
		})
	}
	r := NewRouter()
	r.Get("/hi", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("bye")) })
	r.With(mw1).With(mw2).Get("/inline", func(w http.ResponseWriter, r *http.Request) {
		v1 := r.Context().Value(ctxKey{"inline1"}).(string)
		v2 := r.Context().Value(ctxKey{"inline2"}).(string)
		fmt.Fprintf(w, "inline %s %s", v1, v2)
	})
	mustBody(t, r, "GET", "/hi", "bye")
	mustBody(t, r, "GET", "/inline", "inline yes yes")
	if cmwInit1 != 1 || cmwHandler1 != 1 || cmwInit2 != 1 || cmwHandler2 != 1 {
		t.Fatalf("inline counters: %d %d %d %d", cmwInit1, cmwHandler1, cmwInit2, cmwHandler2)
	}
}

func TestChiMuxHandlePatternValidation(t *testing.T) {
	t.Run("Valid pattern without method", func(t *testing.T) {
		r := NewRouter()
		r.Handle("/user/{id}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte("without-prefix GET"))
		}))
		mustBody(t, r, "GET", "/user/123", "without-prefix GET")
	})
	t.Run("Valid pattern with method", func(t *testing.T) {
		r := NewRouter()
		r.Handle("POST /products/{id}", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte("with-prefix POST"))
		}))
		mustBody(t, r, "POST", "/products/456", "with-prefix POST")
	})
	t.Run("Extended whitespace after method", func(t *testing.T) {
		r := NewRouter()
		r.Handle("PATCH \t /", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte("extended-whitespace PATCH"))
		}))
		mustBody(t, r, "PATCH", "/", "extended-whitespace PATCH")
	})
	t.Run("Pattern without leading slash panics", func(t *testing.T) {
		assertPanics(t, "no slash", func() {
			NewRouter().Handle("INVALID/user/{id}", http.HandlerFunc(ok))
		})
		assertPanics(t, "method-glued", func() {
			NewRouter().Handle("GET/user/{id}", http.HandlerFunc(ok))
		})
	})
	// Chi panics on an unregistered custom method; we accept any method name
	// (documented superset), so "UNSUPPORTED /x" registers instead.
	t.Run("Unknown method is accepted (superset of Chi)", func(t *testing.T) {
		r := NewRouter()
		r.Handle("UNSUPPORTED /x", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte("unsupported"))
		}))
		mustBody(t, r, "UNSUPPORTED", "/x", "unsupported")
	})
}

func TestChiRouterFromMuxWith(t *testing.T) {
	r := NewRouter()
	with := r.With(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, r) })
	})
	with.Get("/with_middleware", func(http.ResponseWriter, *http.Request) {})
	testRequest(t, with, http.MethodGet, "/with_middleware", nil)
}

func TestChiMuxMiddlewareStack(t *testing.T) {
	var stdmwInit, stdmwHandler uint64
	stdmw := func(next http.Handler) http.Handler {
		stdmwInit++
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stdmwHandler++
			next.ServeHTTP(w, r)
		})
	}
	var ctxmwInit, ctxmwHandler uint64
	ctxmw := func(next http.Handler) http.Handler {
		ctxmwInit++
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctxmwHandler++
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{"count.ctxmwHandler"}, ctxmwHandler)))
		})
	}
	var inCtxmwInit, inCtxmwHandler uint64
	inCtxmw := func(next http.Handler) http.Handler {
		inCtxmwInit++
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inCtxmwHandler++
			next.ServeHTTP(w, r)
		})
	}
	r := NewRouter()
	r.Use(stdmw)
	r.Use(ctxmw)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/ping" {
				w.Write([]byte("pong"))
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	var handlerCount uint64
	r.With(inCtxmw).Get("/", func(w http.ResponseWriter, r *http.Request) {
		handlerCount++
		ctxmwHandlerCount := r.Context().Value(ctxKey{"count.ctxmwHandler"}).(uint64)
		fmt.Fprintf(w, "inits:%d reqs:%d ctxValue:%d", ctxmwInit, handlerCount, ctxmwHandlerCount)
	})
	r.Get("/hi", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("wooot")) })

	testRequest(t, r, "GET", "/", nil)
	testRequest(t, r, "GET", "/", nil)
	mustBody(t, r, "GET", "/", "inits:1 reqs:3 ctxValue:3")
	mustBody(t, r, "GET", "/ping", "pong")
}

func TestChiMuxRouteGroups(t *testing.T) {
	var stdmwInit, stdmwHandler uint64
	stdmw := func(next http.Handler) http.Handler {
		stdmwInit++
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stdmwHandler++
			next.ServeHTTP(w, r)
		})
	}
	var stdmwInit2, stdmwHandler2 uint64
	stdmw2 := func(next http.Handler) http.Handler {
		stdmwInit2++
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stdmwHandler2++
			next.ServeHTTP(w, r)
		})
	}
	r := NewRouter()
	r.Group(func(r Router) {
		r.Use(stdmw)
		r.Get("/group", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("root group")) })
	})
	r.Group(func(r Router) {
		r.Use(stdmw2)
		r.Get("/group2", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("root group2")) })
	})
	mustBody(t, r, "GET", "/group", "root group")
	mustBody(t, r, "GET", "/group2", "root group2")
	if stdmwInit2 != 1 || stdmwHandler2 != 1 {
		t.Fatalf("stdmw2 counters: %d:%d", stdmwInit2, stdmwHandler2)
	}
}

func TestChiMuxBig(t *testing.T) {
	r := chiBigMux(t)
	// Divergence: Chi returns 404 for "/folders"; the stdlib ServeMux
	// canonicalizes it to "/folders/" with a redirect, as net/http does.
	if w := do(t, r, "GET", "/folders"); !isRedirect(w.Code) || w.Header().Get("Location") != "/folders/" {
		t.Errorf("GET /folders = %d %q, want a redirect -> /folders/", w.Code, w.Header().Get("Location"))
	}
	for _, tc := range []struct{ method, path, want string }{
		{"GET", "/favicon.ico", "fav"},
		{"GET", "/hubs/4/view", "/hubs/4/view reqid:1 session:anonymous"},
		{"GET", "/hubs/4/view/index.html", "/hubs/4/view/index.html reqid:1 session:anonymous"},
		{"POST", "/hubs/ethereumhub/view/index.html", "/hubs/ethereumhub/view/index.html reqid:1 session:anonymous"},
		{"GET", "/", "/ reqid:1 session:elvis"},
		{"GET", "/suggestions", "/suggestions reqid:1 session:elvis"},
		{"GET", "/woot/444/hiiii", "/woot/444/hiiii"},
		{"GET", "/hubs/123", "/hubs/123 reqid:1 session:elvis"},
		{"GET", "/hubs/123/touch", "/hubs/123/touch reqid:1 session:elvis"},
		{"GET", "/hubs/123/webhooks", "/hubs/123/webhooks reqid:1 session:elvis"},
		{"GET", "/hubs/123/posts", "/hubs/123/posts reqid:1 session:elvis"},
		{"GET", "/folders/", "/folders/ reqid:1 session:elvis"},
		{"GET", "/folders/public", "/folders/public reqid:1 session:elvis"},
		{"GET", "/folders/nothing", "404 page not found\n"},
	} {
		mustBody(t, r, tc.method, tc.path, tc.want)
	}
}

func chiBigMux(t *testing.T) Router {
	t.Helper()
	r := NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{"requestID"}, "1")))
		})
	})
	r.Group(func(r Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{"session.user"}, "anonymous")))
			})
		})
		r.Get("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("fav")) })
		r.Get("/hubs/{hubID}/view", func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			fmt.Fprintf(w, "/hubs/%s/view reqid:%s session:%s", URLParam(r, "hubID"),
				ctx.Value(ctxKey{"requestID"}), ctx.Value(ctxKey{"session.user"}))
		})
		r.Get("/hubs/{hubID}/view/*", func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			fmt.Fprintf(w, "/hubs/%s/view/%s reqid:%s session:%s", URLParamFromCtx(ctx, "hubID"),
				URLParam(r, "*"), ctx.Value(ctxKey{"requestID"}), ctx.Value(ctxKey{"session.user"}))
		})
		r.Post("/hubs/{hubSlug}/view/*", func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			fmt.Fprintf(w, "/hubs/%s/view/%s reqid:%s session:%s", URLParamFromCtx(ctx, "hubSlug"),
				URLParam(r, "*"), ctx.Value(ctxKey{"requestID"}), ctx.Value(ctxKey{"session.user"}))
		})
	})
	r.Group(func(r Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{"session.user"}, "elvis")))
			})
		})
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			fmt.Fprintf(w, "/ reqid:%s session:%s", ctx.Value(ctxKey{"requestID"}), ctx.Value(ctxKey{"session.user"}))
		})
		r.Get("/suggestions", func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			fmt.Fprintf(w, "/suggestions reqid:%s session:%s", ctx.Value(ctxKey{"requestID"}), ctx.Value(ctxKey{"session.user"}))
		})
		r.Get("/woot/{wootID}/*", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "/woot/%s/%s", URLParam(r, "wootID"), URLParam(r, "*"))
		})
		r.Route("/hubs", func(r Router) {
			r.Route("/{hubID}", func(r Router) {
				r.Get("/", func(w http.ResponseWriter, r *http.Request) {
					ctx := r.Context()
					fmt.Fprintf(w, "/hubs/%s reqid:%s session:%s", URLParam(r, "hubID"),
						ctx.Value(ctxKey{"requestID"}), ctx.Value(ctxKey{"session.user"}))
				})
				r.Get("/touch", func(w http.ResponseWriter, r *http.Request) {
					ctx := r.Context()
					fmt.Fprintf(w, "/hubs/%s/touch reqid:%s session:%s", URLParam(r, "hubID"),
						ctx.Value(ctxKey{"requestID"}), ctx.Value(ctxKey{"session.user"}))
				})
				sr3 := NewRouter()
				sr3.Get("/", func(w http.ResponseWriter, r *http.Request) {
					ctx := r.Context()
					fmt.Fprintf(w, "/hubs/%s/webhooks reqid:%s session:%s", URLParam(r, "hubID"),
						ctx.Value(ctxKey{"requestID"}), ctx.Value(ctxKey{"session.user"}))
				})
				sr3.Route("/{webhookID}", func(r Router) {
					r.Get("/", func(w http.ResponseWriter, r *http.Request) {
						ctx := r.Context()
						fmt.Fprintf(w, "/hubs/%s/webhooks/%s reqid:%s session:%s", URLParam(r, "hubID"),
							URLParam(r, "webhookID"), ctx.Value(ctxKey{"requestID"}), ctx.Value(ctxKey{"session.user"}))
					})
				})
				r.With(func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{"hook"}, true)))
					})
				}).Mount("/webhooks", sr3)
				r.Route("/posts", func(r Router) {
					r.Get("/", func(w http.ResponseWriter, r *http.Request) {
						ctx := r.Context()
						fmt.Fprintf(w, "/hubs/%s/posts reqid:%s session:%s", URLParam(r, "hubID"),
							ctx.Value(ctxKey{"requestID"}), ctx.Value(ctxKey{"session.user"}))
					})
				})
			})
		})
		r.Route("/folders/", func(r Router) {
			r.Get("/", func(w http.ResponseWriter, r *http.Request) {
				ctx := r.Context()
				fmt.Fprintf(w, "/folders/ reqid:%s session:%s", ctx.Value(ctxKey{"requestID"}), ctx.Value(ctxKey{"session.user"}))
			})
			r.Get("/public", func(w http.ResponseWriter, r *http.Request) {
				ctx := r.Context()
				fmt.Fprintf(w, "/folders/public reqid:%s session:%s", ctx.Value(ctxKey{"requestID"}), ctx.Value(ctxKey{"session.user"}))
			})
		})
	})
	return r
}

func TestChiMuxSubroutesBasic(t *testing.T) {
	hIndex := func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("index")) }
	hArticlesList := func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("articles-list")) }
	hSearch := func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("search-articles")) }
	hGetArticle := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "get-article:%s", URLParam(r, "id"))
	}
	hSyncArticle := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "sync-article:%s", URLParam(r, "id"))
	}
	r := NewRouter()
	r.Get("/", hIndex)
	r.Route("/articles", func(r Router) {
		r.Get("/", hArticlesList)
		r.Get("/search", hSearch)
		r.Route("/{id}", func(r Router) {
			r.Get("/", hGetArticle)
			r.Get("/sync", hSyncArticle)
		})
	})
	mustBody(t, r, "GET", "/", "index")
	mustBody(t, r, "GET", "/articles", "articles-list")
	mustBody(t, r, "GET", "/articles/search", "search-articles")
	mustBody(t, r, "GET", "/articles/123", "get-article:123")
	mustBody(t, r, "GET", "/articles/123/sync", "sync-article:123")
}

func TestChiMuxSubroutes(t *testing.T) {
	hHubView1 := func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("hub1")) }
	hHubView2 := func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("hub2")) }
	hHubView3 := func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("hub3")) }
	hAccountView1 := func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("account1")) }
	hAccountView2 := func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("account2")) }

	r := NewRouter()
	r.Get("/hubs/{hubID}/view", hHubView1)
	r.Get("/hubs/{hubID}/view/*", hHubView2)
	sr := NewRouter()
	sr.Get("/", hHubView3)
	r.Mount("/hubs/{hubID}/users", sr)
	r.Get("/hubs/{hubID}/users/", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("hub3 override"))
	})
	sr3 := NewRouter()
	sr3.Get("/", hAccountView1)
	sr3.Get("/hi", hAccountView2)
	r.Route("/accounts/{accountID}", func(r Router) {
		r.Mount("/", sr3)
	})

	mustBody(t, r, "GET", "/hubs/123/view", "hub1")
	mustBody(t, r, "GET", "/hubs/123/view/index.html", "hub2")
	mustBody(t, r, "GET", "/hubs/123/users", "hub3")
	mustBody(t, r, "GET", "/hubs/123/users/", "hub3 override")
	mustBody(t, r, "GET", "/accounts/44", "account1")
	mustBody(t, r, "GET", "/accounts/44/hi", "account2")

	// The routing pattern stack is built across the mounted sub-routers.
	rctx := NewRouteContext()
	req := httptest.NewRequest("GET", "/accounts/44/hi", nil).WithContext(
		context.WithValue(t.Context(), RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Body.String() != "account2" {
		t.Fatal(w.Body.String())
	}
	if len(rctx.RoutePatterns) != 3 {
		t.Fatalf("expected 3 routing patterns, got %d: %v", len(rctx.RoutePatterns), rctx.RoutePatterns)
	}
}

func TestChiSingleHandler(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hi " + URLParam(r, "name")))
	})
	rctx := NewRouteContext()
	rctx.params.Add("name", "joe")
	req := httptest.NewRequest("GET", "/", nil).WithContext(context.WithValue(t.Context(), RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Body.String() != "hi joe" {
		t.Fatal(w.Body.String())
	}
}

func TestChiServeHTTPExistingContext(t *testing.T) {
	r := NewRouter()
	r.Get("/hi", func(w http.ResponseWriter, req *http.Request) {
		s, _ := req.Context().Value(ctxKey{"testCtx"}).(string)
		w.Write([]byte(s))
	})
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		s, _ := req.Context().Value(ctxKey{"testCtx"}).(string)
		w.WriteHeader(404)
		w.Write([]byte(s))
	})
	cases := []struct {
		ctx, method, path, body string
		status                  int
	}{
		{"hi ctx", "GET", "/hi", "hi ctx", 200},
		{"nothing here ctx", "GET", "/hello", "nothing here ctx", 404},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil).WithContext(
			context.WithValue(t.Context(), ctxKey{"testCtx"}, tc.ctx))
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.status || w.Body.String() != tc.body {
			t.Fatalf("%s %s: %d %q", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

func TestChiNestedGroups(t *testing.T) {
	handlerPrintCounter := func(w http.ResponseWriter, r *http.Request) {
		counter, _ := r.Context().Value(ctxKey{"counter"}).(int)
		fmt.Fprintf(w, "%v", counter)
	}
	mwIncreaseCounter := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter, _ := r.Context().Value(ctxKey{"counter"}).(int)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{"counter"}, counter+1)))
		})
	}
	r := NewRouter()
	r.Get("/0", handlerPrintCounter)
	r.Group(func(r Router) {
		r.Use(mwIncreaseCounter)
		r.Get("/1", handlerPrintCounter)
		r.With(mwIncreaseCounter).Get("/2", handlerPrintCounter)
		r.Group(func(r Router) {
			r.Use(mwIncreaseCounter, mwIncreaseCounter)
			r.Get("/3", handlerPrintCounter)
		})
		r.Route("/", func(r Router) {
			r.Use(mwIncreaseCounter, mwIncreaseCounter)
			r.With(mwIncreaseCounter).Get("/4", handlerPrintCounter)
			r.Group(func(r Router) {
				r.Use(mwIncreaseCounter, mwIncreaseCounter)
				r.Get("/5", handlerPrintCounter)
				r.With(mwIncreaseCounter).Get("/6", handlerPrintCounter)
			})
		})
	})
	for _, route := range []string{"0", "1", "2", "3", "4", "5", "6"} {
		mustBody(t, r, "GET", "/"+route, route)
	}
}

func TestChiMiddlewarePanicOnLateUse(t *testing.T) {
	assertPanics(t, "late Use", func() {
		r := NewRouter()
		r.Get("/", func(http.ResponseWriter, *http.Request) {})
		r.Use(func(next http.Handler) http.Handler { return next })
	})
}

func TestChiMountingExistingPath(t *testing.T) {
	assertPanics(t, "mount existing", func() {
		r := NewRouter()
		r.Get("/", func(http.ResponseWriter, *http.Request) {})
		r.Mount("/hi", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		r.Mount("/hi", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	})
}

func TestChiMountingSimilarPattern(t *testing.T) {
	r := NewRouter()
	r.Get("/hi", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("bye")) })
	r2 := NewRouter()
	r2.Get("/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("foobar")) })
	r3 := NewRouter()
	r3.Get("/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("foo")) })
	r.Mount("/foobar", r2)
	r.Mount("/foo", r3)
	mustBody(t, r, "GET", "/hi", "bye")
}

func TestChiMountingSelf(t *testing.T) {
	assertPanics(t, "direct self-mount", func() {
		r := NewRouter()
		r.Get("/ping", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("pong")) })
		r.Mount("/", r)
	})
}

func TestChiMuxMissingParams(t *testing.T) {
	r := NewRouter()
	r.Get(`/user/{userId:\d+}`, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "userId = '%s'", URLParam(r, "userId"))
	})
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte("nothing here"))
	})
	mustBody(t, r, "GET", "/user/123", "userId = '123'")
	mustBody(t, r, "GET", "/user/", "nothing here")
}

func TestChiMuxWildcardRoute(t *testing.T) {
	assertPanics(t, "wildcard not at end", func() {
		NewRouter().Get("/*/wildcard/must/be/at/end", func(http.ResponseWriter, *http.Request) {})
	})
	assertPanics(t, "wildcard before param", func() {
		NewRouter().Get("/*/wildcard/{must}/be/at/end", func(http.ResponseWriter, *http.Request) {})
	})
}

func TestChiMuxRegexp2(t *testing.T) {
	r := NewRouter()
	r.Get("/foo-{suffix:[a-z]{2,3}}.json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(URLParam(r, "suffix")))
	})
	mustBody(t, r, "GET", "/foo-.json", "404 page not found\n")
	mustBody(t, r, "GET", "/foo-abc.json", "abc")
}

func TestChiMuxRegexp3(t *testing.T) {
	r := NewRouter()
	r.Get("/one/{firstId:[a-z0-9-]+}/{secondId:[a-z]+}/first", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("first"))
	})
	r.Get("/one/{firstId:[a-z0-9-_]+}/{secondId:[0-9]+}/second", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("second"))
	})
	r.Delete("/one/{firstId:[a-z0-9-_]+}/{secondId:[0-9]+}/second", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("third"))
	})
	r.Route("/one", func(r Router) {
		r.Get("/{dns:[a-z-0-9_]+}", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("_")) })
		r.Get("/{dns:[a-z-0-9_]+}/info", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("_")) })
		r.Delete("/{id:[0-9]+}", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("forth")) })
	})
	mustBody(t, r, "GET", "/one/hello/peter/first", "first")
	mustBody(t, r, "GET", "/one/hithere/123/second", "second")
	mustBody(t, r, "DELETE", "/one/hithere/123/second", "third")
	mustBody(t, r, "DELETE", "/one/123", "forth")
}

func TestChiMuxSubrouterWildcardParam(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "param:%v *:%v", URLParam(r, "param"), URLParam(r, "*"))
	}
	r := NewRouter()
	r.Get("/bare/{param}", h)
	r.Get("/bare/{param}/*", h)
	r.Route("/case0", func(r Router) {
		r.Get("/{param}", h)
		r.Get("/{param}/*", h)
	})
	mustBody(t, r, "GET", "/bare/hi", "param:hi *:")
	mustBody(t, r, "GET", "/bare/hi/yes", "param:hi *:yes")
	mustBody(t, r, "GET", "/case0/hi", "param:hi *:")
	mustBody(t, r, "GET", "/case0/hi/yes", "param:hi *:yes")
}

func TestChiMuxContextIsThreadSafe(t *testing.T) {
	router := NewRouter()
	router.Get("/{id}", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		<-ctx.Done()
	})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				req := httptest.NewRequest("GET", "/ok", nil).WithContext(t.Context())
				ctx, cancel := context.WithCancel(req.Context())
				go cancel()
				router.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
			}
		}()
	}
	wg.Wait()
}

func TestChiCustomHTTPMethod(t *testing.T) {
	RegisterMethod("BOO")
	r := NewRouter()
	r.Get("/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(".")) })
	r.MethodFunc("BOO", "/hi", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("custom method")) })
	mustBody(t, r, "GET", "/", ".")
	mustBody(t, r, "BOO", "/hi", "custom method")

	expect := map[string]string{"GET": "/", "BOO": "/hi"}
	seen := 0
	if err := Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		want, ok := expect[method]
		if !ok {
			t.Fatalf("unexpected method %s", method)
		}
		if want != route {
			t.Fatalf("method %s: got %s want %s", method, route, want)
		}
		seen++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Fatalf("expected 2 routes, got %d", seen)
	}
}
