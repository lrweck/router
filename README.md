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
| `/x` with only `/x/` registered | 404 | redirect to `/x/`, exactly as net/http does |
| `//` or `.` in the path | routes with empty params | redirect (canonicalization), as net/http does |
| param with `%2f` | returns the escaped value | returns it decoded |
| 405 `Allow` | one header per method | a single `"GET, HEAD, POST"` |
| `HEAD` | needs `Head`/`GetHead` | served from `GET` |

`net/http`'s routing behavior is the product of a lot of careful work; we follow
it rather than second-guess it.

- **Hacker's Delight** — the SWAR (SIMD-within-a-register) byte-class scanner
  follows the classic bit tricks, with carry-free comparisons (the usual
  `hasless` propagates borrows and breaks when masks are combined).

## The tricks

None of this is magic — it is the result of profiling and cutting what doesn't
pay off. Everything below is measured in [`bench/README.md`](bench/README.md).

### In `Compat` (on top of `ServeMux`)

- **Native param names.** The placeholder in the `ServeMux` pattern is the name
  you wrote (`/users/{id}`), so `net/http` itself fills `r.PathValue("id")` — no
  `SetPathValue` and none of the `otherValues` map that `net/http` allocates
  for names that aren't in the pattern.
- **Lazy routing Context.** A plain route (static segments and native
  `{param}`) allocates **no context at all**: `Param(r, "id")` reads the
  stdlib's path value directly. The Context is only created when a route really
  needs it — mounts (params accumulate across sub-routers), wildcards, mixed
  segments (`{a}-{b}`) or a renamed param.
- **A single match.** With no custom `NotFound`/`MethodNotAllowed`, `ServeHTTP`
  goes straight to `ServeMux.ServeHTTP`; there is no `mux.Handler` probe first.
- **Catch-all + a first-segment filter.** The stdlib, on a 404/405, walks the
  whole tree to compute the `Allow` list (`matchingMethods`). We register a
  least-specific `"/"` handler so the miss lands on us instead, and answer with
  an O(1) check on the first static segment. That is why our 404 is ~14 ns while
  the raw stdlib's is ~546 ns.
- **Params on the stack.** `extract` writes into a fixed array in the caller's
  frame. Passing the buffer in, rather than returning a slice, keeps it on the
  stack — no 256 B heap allocation per request.
- **The middleware chain is built once**, at `freeze()` (the first
  route/`With`/`Group`/`Route`/`Mount`), not on the first request — so the
  constructors run exactly once and the chain is read-only while serving.

### In `Mux` (the purpose-built trie)

- **Match by comparing bytes against the path.** A static child is matched with
  `path[start:start+len(key)] == key` plus a segment-boundary check — no
  segment slicing, no `/` scan.
- **Children: inline up to 4, then a first-byte bucket.** Low fan-out is a
  linear compare over up to four children (faster than hashing a short key).
  Beyond that they live in a sorted slice with a 257-entry first-byte index
  (O(1) to the bucket) and a binary search inside it. A per-segment radix and a
  full-key binary search both **measure slower** here (see `mux.go`).
- **`Params` by value, keys shared per route.** Parameters are a fixed array
  passed by value (no allocation), with the names in a pointer shared by the
  route — and stored **per method**, so `GET /u/{id}` and `POST /u/{name}` can
  coexist on the same shape.
- **Backtracking only where it's needed.** Static edges are walked in a loop;
  recursion (with parameter rollback) is reserved for param/mixed/wildcard
  alternatives, and constraints are ordered most-specific-first.
- **`Allow` precomputed at registration**, and the 405 (like the 404) writes no
  body: status + header only.
- **`nextSlash`** uses `strings.IndexByte` (the runtime's SIMD `memchr`) for
  remainders of 16+ bytes and a byte loop below that, where the call isn't worth
  it.

### In the constraints (without `regexp`)

- **Single repeated class** (`[0-9]+`, `[a-z0-9-]+`, `\d{4}`) compiles to one
  `bytesInClass` call over the value.
- **Fixed-shape sequences** — UUID, dates (`[0-9]{4}-[0-9]{2}-[0-9]{2}`),
  versions, IPs — compile to a **linear pass**: literals and fixed-count classes
  matched left to right, no backtracking, no allocation. A UUID constraint
  validates in ~120 ns this way.
- **SWAR / SIMD** in `bytesInClass`: 8 bytes per iteration with `uint64`
  arithmetic in the default build; the portable `simd` package (32-byte
  vectors) under `GOEXPERIMENT=simd`; SWAR again for the tail, for short inputs
  and when SIMD is emulated. Classes with a byte ≥128 fall back to the plain
  loop.
- **Literal alternation** (`(asc|desc)`) uses explicit, allocation-free
  backtracking.
- **The generic fallback** keeps a step *budget*, so a pathological pattern
  fails the match (404) instead of hanging the server — a deliberate ceiling.

## Reality check

Routing is a **tiny** part of a real request. A route match here costs tens of
nanoseconds; a database query, a file read or an outbound HTTP call costs
**microseconds to milliseconds**. In other words: **the moment your handler
touches I/O, any saving you got from the router disappears into the noise** —
you'd have to save the entire router cost hundreds of times over to matter.

So pick a router for its API, its correctness and its behavior, not for the
benchmark. Performance is a tie-breaker between options you already like, not a
reason to change your architecture. The numbers in `bench/` are there to show
the work is honest, not to promise your service will be faster.

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
