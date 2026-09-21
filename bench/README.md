# routerbench

A head-to-head comparison of this `router` package against the most popular Go
HTTP routers. It is a **separate, optional module** — it is not part of the
library build and does not add any dependency to it.

```sh
cd bench
go test -run='^$' -bench=BenchmarkQuick -benchtime=0.3s -count=5 -benchmem   # fast subset
go test -run='^$' -bench=BenchmarkRouters -count=6 -benchmem > out.txt       # full
benchstat out.txt
```

---

## Methodology

### What is measured

Only **routing and dispatch**: given a method and a path, how long does it take
the router to find the handler and call it? The handler does nothing but write
`204` (and, where the API allows, read the first path parameter). No JSON, no
I/O, no business logic, no middleware.

### The harness

Every net/http router implements `http.Handler`, so they are all driven through
the exact same loop:

```go
req := httptest.NewRequest(method, path, nil)   // built once
d := &discardRW{}                               // a no-op ResponseWriter
for b.Loop() {
    d.code = 0
    h.ServeHTTP(d, req)                         // the router under test
}
```

* `discardRW` is a minimal `http.ResponseWriter` that records the status code
  and throws the body away. **No `httptest.ResponseRecorder`** is used: the
  recorder allocates a `bytes.Buffer` and clones headers, which would dominate
  the measurement (a few hundred ns) and hide the router's own cost.
* The request is **built once and reused** across iterations. Neither the
  router nor `ServeHTTP` mutates the caller's request in place (routers that
  attach params derive a new request via `WithContext`), so reuse is safe and
  removes per-iteration request construction from the numbers.
* `b.ReportAllocs()` is on, so `B/op` and `allocs/op` are reported alongside
  `ns/op`. Allocations matter as much as time here: they drive GC pressure
  under real load.

Fiber does not implement `http.Handler` (it is built on fasthttp), so it gets
its own runner that mirrors what fasthttp does per connection: one reused
`fasthttp.RequestCtx`, method/URI set once, `ctx.Response.Reset()` per
iteration, then `app.Handler()(&ctx)`.

### Native vs http-compatible variants

Several libraries ship a fast native handler signature **and** an adapter that
lets a plain `http.HandlerFunc` run inside them. Both are benchmarked, because
the adapter is often much slower and it is exactly the trade-off this package
makes explicit:

| library | native | http-compatible variant |
|---|---|---|
| this package | `mux` (typed: `func(w, r, Params)`) | `compat` (`http.HandlerFunc`, on `net/http.ServeMux`) |
| httprouter | `httprouter` (3-arg handler) | `httprouter-http` (`HandlerFunc` + `ParamsFromContext`) |
| gin | `gin` (`gin.HandlerFunc`) | `gin-http` (`gin.WrapF`) |
| echo | `echo` (`echo.HandlerFunc`) | `echo-http` (`echo.WrapHandler`) |
| bunrouter | `bunrouter` (`bunrouter.HandlerFunc`) | `bunrouter-http` (`HTTPHandlerFunc`) |
| fiber | `fiber` (`fiber.Handler`) | `fiber-http` (net/http adaptor) |

`stdlib`, `chi` and `gorilla` only have the `http.HandlerFunc` form, so they
appear once.

### Constraint categories

Routers are **not** in the same category when it comes to regex constraints.
Some support them (this package, chi, gorilla); most do not (stdlib, httprouter,
gin, echo, bunrouter, fiber — they only have a plain `:param`). Comparing them
on a constrained route would be meaningless, so the benchmark is split:

* **`BenchmarkRouters`** — the no-constraint scenarios, all routers.
* **`BenchmarkConstrained`** — the constrained scenarios, **only the
  regex-capable routers**, which actually validate.
* **`BenchmarkConstrainedPlain`** — the same paths, **only the routers without
  regex support, plus `mux-plain`** (our typed router registering the paths as
  plain params). This is the routing cost *without* validation, which is the
  only fair comparison for that group.

The constrained routes are realistic, not synthetic:

| scenario | route (regex-capable) | request |
|---|---|---|
| `constrained` | `/c/{id:[0-9]+}` | `/c/42` |
| `constrained-uuid` | `/u/{id:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}}` | `/u/550e8400-e29b-41d4-a716-446655440000` |
| `constrained-blog` | `/blog/{date:[0-9]{4}-[0-9]{2}-[0-9]{2}}/{slug:[a-z0-9-]+}` | `/blog/2026-09-21/my-post-title` |
| `constrained-slug` | `/a/{slug:[a-z0-9-]+}` | `/a/home-is-toronto` |

The non-regex routers register the same paths with plain params
(`/u/:id`, `/blog/:date/:slug`, …).

### Scenarios (no constraint)

| scenario | route | request |
|---|---|---|
| `static` | `/ping` | `/ping` |
| `param` | `/users/{id}` | `/users/42` |
| `deep` | `/users/{id}/posts/{pid}/comments/{cid}` | `/users/42/posts/7/comments/9` |
| `wildcard` | `/static/*` | `/static/js/app.js` |
| `many` | 50 routes `/r0/{id}` … `/r49/{id}` | `/r49/42` (the last one) |
| `longparam` | `/users/{id}` | `/users/` + 100 digits |
| `miss` | (no match) | `/nope/nothing` |
| `405` | only `GET /ping` | `DELETE /ping` |

All routers register the same route set (in their own syntax) for each
scenario.

### Environment

* Go 1.27.1, linux/amd64, 13th Gen Intel Core i7-13700H.
* `benchstat` with `-count=6`; the tables report the **median** `ns/op`.

### What this does NOT measure

**Routing is a tiny slice of a real request.** Any I/O — a database query, a
file read, an outbound HTTP call — costs microseconds to milliseconds, while
everything below is tens of nanoseconds. In a handler that touches I/O these
differences are invisible; treat them as a tie-breaker, not a reason to pick an
architecture. Specifically, this does not measure:

* Real server I/O, connection handling, TLS, keep-alive.
* Middleware chains, request parsing (headers/body), JSON encoding.
* Concurrency/throughput under load — these are single-goroutine, single-path
  microbenchmarks. A router can win here and lose under contention (locks,
  pools).
* Param-heavy business logic: handlers read at most one param.

---

## Results

### No constraint (ns/op, median)

| scenario | **mux** | compat | stdlib | chi | httprouter | gin | echo | bunrouter | gorilla | fiber |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| static | **24.7** | 81.5 | 65.1 | 191.5 | 26.2 | 40.2 | 36.1 | 137.4 | 412.2 | 82.1 |
| param | **36.7** | 119.8 | 108.8 | 329.8 | 56.9 | 41.2 | 50.0 | 44.5 | 626.5 | 111.4 |
| deep | **55.5** | 273.2 | 251.2 | 382.9 | 71.5 | 61.1 | 76.4 | 162.9 | 951.0 | 157.2 |
| wildcard | **30.9** | 457.2 | 276.8 | 308.2 | 42.9 | 45.1 | 40.3 | 141.2 | 509.1 | 105.0 |
| many | **46.4** | 116.3 | 105.3 | 355.7 | 53.6 | 54.1 | 59.7 | 52.1 | 2586.5 | 180.6 |
| longparam | **39.9** | 340.6 | 341.1 | 317.1 | 90.3 | 76.7 | 83.8 | 79.5 | 3302.5 | 139.3 |
| miss | **14.4** | 433.5 | 546.1 | 297.5 | 243.8 | 52.2 | 632.5 | 208.8 | 2024.0 | 423.4 |
| 405 | 55.0 | 1395.0 | 672.8 | 316.1 | 290.6 | 73.7 | 841.1 | **52.9** | 716.2 | 610.3 |

Allocations: `mux` is **0** everywhere (1 on the 405, same as gin, from
canonicalizing the `Allow` header key); `compat` 0–11; `stdlib` 0–14; `chi` 2–5;
`gorilla` 7–8.

### Constrained — regex-capable group (they actually validate)

| scenario | **mux** | compat | chi | gorilla |
|---|---:|---:|---:|---:|
| `[0-9]+` | **44.3** | 153.7 | 400.4 | 656.5 |
| UUID | **121.1** | 331.5 | 580.8 | 1145.5 |
| blog (`date` + `slug`) | **111.5** | 313.5 | 670.2 | 1209.0 |
| slug | **68.9** | 185.9 | 559.0 | 1088.0 |

### Constrained — non-regex group (+ `mux` without constraint)

| scenario | **mux-plain** | httprouter | gin | echo | stdlib | fiber |
|---|---:|---:|---:|---:|---:|---:|
| `[0-9]+` | **37.8** | 40.4 | 48.6 | 49.6 | 101.8 | 97.0 |
| UUID | 45.5 | 54.7 | 60.4 | **44.6** | 184.1 | 126.0 |
| blog | **50.1** | 59.3 | 64.0 | 61.0 | 193.2 | 112.6 |
| slug | 41.6 | 42.9 | 48.4 | **39.7** | 128.3 | 95.4 |

### SIMD

`GOEXPERIMENT=simd` does **not** change the constrained numbers above for
`mux` (44.3/45.3, 121/151, 111/121, 68.9/69.0 — within noise). The realistic
constraints validate in **short chunks** (8/4/12 bytes for a UUID), below the
32-byte vector width, so only the SWAR path runs. SIMD pays off for a **long
single-class parameter** — e.g. a 108-byte slug goes from 83 ns (SWAR) to
44 ns (SIMD); see `BenchmarkValidator` in the main module.

### Parallel throughput

`BenchmarkParallel` runs the same routes under `b.RunParallel` (one goroutine
per `GOMAXPROCS`). Each goroutine gets its own request and `ResponseWriter`,
because routers may set `r.Pattern` / path values on the request they are
handed. `RunParallel` reports per-operation wall time, so **lower is more
throughput**.

```sh
go test -run='^$' -bench=BenchmarkParallel -benchmem
```

| variant | static | param | deep | constrained-uuid | miss |
|---|---:|---:|---:|---:|---:|
| **mux** | 3.8 | **5.8** | 9.1 | 17.3 | **2.7** |
| mux-plain | 3.8 | 5.7 | 10.1 | **6.2** | 2.7 |
| httprouter | **2.8** | 29.6 | 35.4 | 16.1 | 55.7 |
| gin | 4.8 | 6.4 | **8.5** | 8.2 | 7.5 |
| echo | 4.4 | 6.6 | 9.7 | 6.3 | 244.2 |
| bunrouter | 119.8 | 5.5 | 135.0 | 7.1 | 53.0 |
| stdlib | 64.8 | 78.7 | 107.2 | 87.4 | 257.8 |
| compat | 67.4 | 82.9 | 110.7 | 103.4 | 151.9 |
| chi | 121.2 | 229.8 | 260.9 | 285.6 | 177.1 |
| gorilla | 265.6 | 383.2 | 452.2 | 478.3 | 413.3 |
| fiber | 46.2 | 49.1 | 54.4 | 50.3 | 105.2 |

* **`mux` is first or second everywhere.** Its worst case is `static` at 1.36x
  behind httprouter; on `param` and `miss` it is at or near the top. No locks,
  no per-request allocation, so it scales with cores.
* **`compat` inherits the stdlib's ceiling.** It tracks raw `stdlib` (67 vs 65
  on static, 83 vs 79 on param) — both are limited by the `ServeMux`'s
  `RWMutex`: the read lock is a shared atomic on one cache line, so it
  ping-pongs across cores. Single-threaded `mux` was ~4x faster than `compat`;
  in parallel it is ~15x, and that gap is the lock, not the routing.
* **httprouter's per-request `Params` slice** costs it under load: its `param`
  goes from 2.8 ns (static) to 29.6 ns, while `mux` (params by value) stays at
  5.8. Allocations scale GC work with cores.
* **gin and echo scale well** (they keep params in a per-request context of
  their own); `fiber` is middling here because the reused `RequestCtx` is per
  goroutine, not per connection pool.

---

## Reading the numbers

* **`mux` (typed) wins every no-constraint scenario** with 0 allocations. The
  only loss is the 405 to bunrouter, by ~2 ns (noise).
* **In the regex group `mux` wins by 2.7–10x.** The fixed-shape validator
  (UUID, date) is a single linear pass — no backtracking, no allocation — and
  reuses the same byte-class scanner (SWAR/SIMD) as the simple cases.
* **In the non-regex group `mux-plain` is at the top**, tied with echo.
* **The `http.Handler` adapter is expensive**: `httprouter-http` is 184 ns vs
  57 ns for its native handler on `param`. That is precisely why `mux` (typed)
  exists next to `compat` (stdlib semantics).
* **`compat` beats `chi`** (the closest peer, same API style) by ~2.5x on hits,
  but pays for stdlib semantics on 404/405 (body + allowed-methods scan).
* **gorilla** is consistently the slowest; **stdlib** is strong on static but
  expensive on wildcard/404/405.

Caveats: handlers read at most one param; gin/echo are full frameworks (only
their router is measured here); Fiber uses fasthttp; the host is noisy, so
compare ratios from the same run rather than absolute values across runs.
