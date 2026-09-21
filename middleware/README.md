# middleware

Optional middlewares, one package per middleware, under
`github.com/lrweck/router/middleware/<name>`.

They are **opt-in**: you only get a middleware if you import its package, and
**none of them add a dependency** — the whole module stays standard-library
only (there is a test, `TestStdlibOnly`, that fails if a `require` ever appears
in `go.mod`). A middleware that needed a third-party library would live in its
own module, with its own tag, so the core stays clean.

Every middleware has the signature `func(http.Handler) http.Handler`, the same
type as `router.Middleware` and Chi's. It works with `Compat`, with `Mux`, and
with a plain `net/http` server.

```go
r := router.NewRouter() // or router.NewMux()
r.Use(requestid.New(), recoverer.New(nil), logger.New(nil))
```

## Fast path on `Mux`

A root middleware added with `Use` wraps the mux and **keeps the
allocation-free path** (the typed handler still gets its `Params` as
arguments). It can read the matched pattern from `r.Pattern` after `next`, but
not the path parameters, because no routing Context is created. A middleware
that needs the parameters goes on the route with `With`/`Group`, which uses the
pooled Context:

```go
m.Use(logger.New(nil))                        // 0 allocs, sees r.Pattern
m.With(authorize).Get("/x/{id}", handler)     // route-scoped, sees Param(r, "id")
```

## The middlewares

| package | what it does |
|---|---|
| [`requestid`](requestid) | assigns `X-Request-Id`, reuses a sane incoming one, exposes it via `From`/`FromContext` |
| [`logger`](logger) | one `slog` line per request: method, path, pattern, status, bytes, duration, request id |
| [`recoverer`](recoverer) | recovers a panic into a 500 and logs the stack; re-panics `http.ErrAbortHandler` |
| [`realip`](realip) | rewrites `RemoteAddr` from RFC 7239 `Forwarded`, else the de-facto forwarding headers |
| [`timeout`](timeout) | puts a deadline on the request context |
| [`nocache`](nocache) | marks the response uncacheable |
| [`compress`](compress) | gzips the response when RFC 9110 `Accept-Encoding` allows it |
| [`throttle`](throttle) | bounds concurrency (backpressure; 503 if the context is canceled) |
| [`basicauth`](basicauth) | RFC 7617 HTTP Basic auth, with a `Validator` hook for hashed passwords |
| [`cors`](cors) | CORS per the Fetch standard (Origin per RFC 6454), preflight included |

## Headers follow the RFCs

Header handling is grounded in the standards, not in ad-hoc parsing:

- **Accept-Encoding / qvalues** — RFC 9110 §12.5.3 and §12.4.2 (`*`, `q=0`,
  the qvalue grammar). See `compress`.
- **Forwarded** — RFC 7239 (`for=`, quoted nodes, IPv6 brackets, obfuscated
  identifiers). The legacy `X-Forwarded-For`/`X-Real-IP` are de-facto and used
  only as a fallback. See `realip`.
- **Basic** — RFC 7617, with the `WWW-Authenticate` realm as an RFC 9110
  quoted-string. See `basicauth`.
- **CORS** — the WHATWG Fetch standard; `Origin` is RFC 6454. See `cors`.
- **Request IDs** — no RFC exists; `X-Request-Id` is de-facto (there is an IETF
  draft). The value is validated as an RFC 9110 token. See `requestid`.
