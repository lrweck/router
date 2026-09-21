# router

An HTTP router for Go with the **[Chi](https://github.com/go-chi/chi) API** on
top of the standard library, with **zero dependencies**. The idea is simple: you
write routes the Chi way (`r.Get("/users/{id}", ...)`, `r.Route(...)`,
`r.Group(...)`, `URLParam(r, "id")`), but the actual work is done by
`net/http.ServeMux` — or, when you want the fastest path, by a purpose-built
segment trie with typed handlers.

## Why this exists

Go's `net/http.ServeMux` (1.22+) already routes by method and wildcard, and it
is fast. Chi, on the other hand, has a far nicer API (groups, sub-routers, named
params), but it carries its own radix tree and its own set of behavioral rules.

This library gives you both, without choosing for you:

- **`Compat`** is the Chi API on top of `ServeMux`. Because the routing is the
  stdlib's, you inherit its semantics: path canonicalization with redirects,
  decoded param values, the `Allow` header format, `HEAD` served from `GET`.
  This is the default engine (`NewRouter()` is an alias for `NewCompat()`).
- **`Mux`** is its own engine (a segment trie) with **typed handlers**, where
  params arrive as arguments. This is the fast path: **zero allocations per
  request**.

Both share the same pattern syntax — including Chi-style regex constraints,
`{id:[0-9]+}`, implemented without the `regexp` package.

```go
// Compat: the Chi API, with stdlib semantics.
r := router.NewRouter()
r.Use(logger) // func(http.Handler) http.Handler, the same type as Chi's
r.Route("/articles", func(r router.Router) { // an interface, like chi.Router
	r.Get("/{id}", func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprint(w, router.Param(req, "id")) // primary; URLParam is an alias
	})
})

// Mux: typed and allocation-free.
m := router.NewMux()
m.Get("/users/{id:[0-9]+}", func(w http.ResponseWriter, req *http.Request, ps router.Params) {
	fmt.Fprint(w, ps.Get("id"))
})
```

## Inspirations and credits

**This project stands on other people's work, and the debt is mostly to Chi.**
The full, per-project attribution (with licenses) is in
[`NOTICE.md`](NOTICE.md); the short version:

- **[Chi](https://github.com/go-chi/chi)** — **the biggest debt by far.** The
  whole public API is a deliberate reimplementation of Chi's (`NewRouter`,
  `Get`/`Post`/…, `Use`/`With`/`Group`/`Route`/`Mount`, `URLParam`,
  `RouteContext`, `Walk`/`Routes`, the `Router` interface in closures), the
  `{name}`/`{name:regexp}`/`*` pattern syntax is Chi's, and the behavioral test
  suite in [`chi_parity_test.go`](chi_parity_test.go) is **ported from Chi's
  `mux_test.go`** (Chi is MIT — its license is reproduced in full in
  `NOTICE.md`, as required for a derivative of its tests). The goal is that Chi
  code runs here unchanged.
- **[httprouter](https://github.com/julienschmidt/httprouter)** — the `Mux`
  engine's central idea (a tree router whose handler takes the params as
  arguments, so a match allocates nothing). Reimplemented, not copied.
- **[matchit](https://github.com/ibraheemdev/matchit)** (Rust/axum) — the
  static-first matching and the "compare directly against the path, prefer the
  most likely branch" approach.
- **`net/http`** — the behavior. Where Chi and the stdlib disagree, **the stdlib
  wins**, and that is pinned in `stdlib_behavior_test.go`:

| case | Chi | here (= stdlib) |
|---|---|---|
| `/x` with only `/x/` registered | 404 | redirect → `/x/` (301/307) |
| `//` or `.` in the path | routes with empty params | redirect (canonicalization) |
| param with `%2f` | returns the escaped value | returns it decoded |
| 405 `Allow` | one header per method | a single `"GET, HEAD, POST"` |
| `HEAD` | needs `Head`/`GetHead` | served from `GET` |

- **Hacker's Delight** — the SWAR (SIMD-within-a-register) byte-class scanner
  follows the classic bit tricks, with carry-free comparisons (the usual
  `hasless` propagates borrows and breaks when masks are combined).

## The tricks

None of this is magic — it is the result of measuring and cutting what doesn't
pay off.

**In `Compat` (on top of `ServeMux`):**

- **Native param names.** The placeholder in the `ServeMux` pattern is the name
  you wrote, so `net/http` fills `r.PathValue("id")` for free — no need to
  create `SetPathValue`'s `otherValues` map.
- **Lazy context.** Plain routes (static and native `{param}`) do not allocate
  the routing Context per request. It only exists when the route needs it:
  mounts, wildcards, mixed segments (`{a}-{b}`), or a renamed param.
- **Catch-all + filter.** A least-specific `"/"` handler receives the misses,
  and an O(1) filter on the first static segment avoids the method scan
  (`matchingMethods`) that `ServeMux` does on 404/405.
- **A single match.** With no custom `NotFound`/`MethodNotAllowed`, there is no
  double probe: just one `ServeMux.ServeHTTP`.

**In `Mux` (the purpose-built trie):**

- **Compare directly against the path.** A static child is matched by comparing
  the path bytes, with no segment extraction and no `/` scan.
- **First-byte bucket.** Beyond 4 children they live in a sorted slice with a
  first-byte index (O(1) to the bucket) plus a binary search inside it —
  measurably faster than a map for short segment keys.
- **`Params` by value.** Params live in a fixed array passed by value, with the
  keys in a pointer shared per route: **0 allocs**, and each method can use
  different param names for the same shape.
- **Lean 404/405.** `Allow` is precomputed at registration, and the 405 (like
  the 404) writes no body — just status + header.

**In the constraints (without `regexp`):**

- **A linear validator for fixed shapes.** UUID, date
  (`[0-9]{4}-[0-9]{2}-[0-9]{2}`), version, IP — sequences of literals and
  fixed-size classes — become a single linear pass, no backtracking and no
  allocation. (This took a UUID from ~1400 ns to ~120 ns.)
- **SWAR / SIMD.** The per-byte class check uses SWAR (8 bytes per iteration
  with `uint64` arithmetic, carry-free comparisons) in the default build, and
  the portable `simd` package when compiled with `GOEXPERIMENT=simd`. Where the
  vector can't run (a tail shorter than one vector, short input, emulated SIMD)
  SWAR takes over. Classes with a byte ≥128 fall back to the plain loop.
- **Literal alternation** (`(asc|desc)`) uses explicit, allocation-free
  backtracking, and the generic matcher has a step *budget* so a pathological
  pattern can't become a DoS vector.

## Performance

The full comparison against **httprouter, chi, gin, echo, bunrouter, gorilla and
fiber** (method, harness and caveats) is in
[`bench/README.md`](bench/README.md). The summary:

| scenario | **mux** | httprouter | gin | echo | stdlib | chi |
|---|---:|---:|---:|---:|---:|---:|
| static | **24.7** | 26.2 | 40.2 | 36.1 | 65.1 | 191.5 |
| param | **36.7** | 56.9 | 41.2 | 50.0 | 108.8 | 329.8 |
| deep | **55.5** | 71.5 | 61.1 | 76.4 | 251.2 | 382.9 |
| wildcard | **30.9** | 42.9 | 45.1 | 40.3 | 276.8 | 308.2 |
| 404 | **14.4** | 243.8 | 52.2 | 632.5 | 546.1 | 297.5 |
| allocs | **0** | 0–1 | 0 | 0 | 0–14 | 2–5 |

`Mux` wins every no-constraint scenario, with 0 allocs; in the group that
**actually validates constraints** (us, chi, gorilla) it wins by 2.7–10x.
`Compat` beats Chi — the closest peer, same API proposition — by ~2.5x on hits,
allocating the same as raw stdlib.

## Who this is for

- People who like the **Chi API** but want **stdlib semantics** and **zero
  dependencies** (useful in code that goes through security review, or when you
  want a router that behaves like `net/http`).
- People who want **top performance** with typed handlers (`Mux`): 0 allocs and
  faster than httprouter/gin/echo on the route path.
- People who want **constraints** (UUID, date, slug) without `regexp` — with
  good performance, too.

## When it does NOT make sense

- If you already use Chi and depend on **its ecosystem** (the `middleware`
  package, `docgen`, `render`, …), stay on Chi. Chi middlewares plug in here,
  but the subpackages do not exist.
- If you want the **full feature set on the fast engine**: `Mux` today has
  `Get/Post/Put/Handle`, the `GetFunc/HandleFunc` adapters and
  `NotFound`/`MethodNotAllowed` — but it does **not** yet have
  `Use/Group/Route/Mount`. For groups and sub-routers, use `Compat`.
- If your goal is to **serve as fast as possible and you accept a framework**
  (gin, echo, fiber), they solve a lot beyond routing — and Fiber, being
  fasthttp, has a different server model.
- If you need **full regex** on any router: here the constraints cover the
  common subset (classes, literals, `?*+{n,m}`, `(a|b)`) and the rest **panics at
  registration**; Chi accepts the whole of RE2.
- If all you need is simple static routes, plain `ServeMux` is enough — you
  don't need a library at all.

## Status and known limitations

- `RouteContext`/`RoutePattern()` are **not** always non-nil like Chi's: plain
  routes do not allocate a context. Use `router.Param(r, name)` and
  `req.Pattern` as the entry point; the context exists when the route needs it.
- `Mux` is its own engine: it does **not** promise stdlib semantics (the default
  404 has an empty body, for example). `Compat` is what preserves them.
- A cap of **8 params** per route (the `Params` type is a fixed array).
- The generic constraint matcher has a *budget*: an excessively pathological
  pattern fails the match (404) instead of hanging the server — a deliberate
  ceiling.
