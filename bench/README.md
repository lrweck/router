# routerbench

A head-to-head comparison of this `router` package against the most popular Go
HTTP routers. It is a **separate, optional module** — it is not part of the
library build and does not add any dependency to it.

```sh
cd bench
go test -run='^$' -bench=BenchmarkQuick -benchtime=0.3s -count=5 -benchmem   # fast subset
go test -run='^$' -bench=BenchmarkRouters -benchtime=0.5s -count=6 -benchmem > out.txt   # full
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
| static | **18.0** | 71.5 | 58.0 | 182.5 | 21.0 | 35.5 | 34.0 | 140.0 | 389.0 | 83.0 |
| param | **32.0** | 114.0 | 96.5 | 327.5 | 57.0 | 41.0 | 50.5 | 44.0 | 588.0 | 97.0 |
| deep | **52.0** | 252.5 | 237.0 | 387.0 | 69.5 | 55.0 | 72.5 | 166.0 | 871.0 | 157.5 |
| wildcard | **24.0** | 436.5 | 248.0 | 297.5 | 38.0 | 40.0 | 36.0 | 139.5 | 471.5 | 87.0 |
| many | **42.0** | 109.5 | 93.0 | 334.0 | 49.5 | 56.5 | 50.0 | 43.0 | 2321.5 | 153.5 |
| longparam | **34.0** | 319.5 | 312.0 | 324.5 | 87.0 | 68.5 | 72.0 | 75.0 | 2787.5 | 131.5 |
| miss | **17.0** | 398.0 | 500.5 | 290.0 | 227.5 | 52.0 | 578.5 | 184.5 | 1999.5 | 396.5 |
| 405 | **44.0** | 1378.0 | 623.0 | 283.0 | 264.0 | 66.0 | 799.5 | 46.0 | 761.0 | 564.5 |

Allocations: `mux` is **0** everywhere (1 on the 405, same as gin, from
canonicalizing the `Allow` header key); `compat` 0–11; `stdlib` 0–14; `chi` 2–5;
`gorilla` 7–8.

### Constrained — regex-capable group (they actually validate)

| scenario | **mux** | compat | chi | gorilla |
|---|---:|---:|---:|---:|
| `[0-9]+` | **37.0** | 136.0 | 388.5 | 614.0 |
| UUID | **111.5** | 320.5 | 544.0 | 1105.0 |
| blog (`date` + `slug`) | **87.5** | 293.0 | 605.0 | 1126.5 |
| slug | **48.0** | 174.5 | 512.5 | 1041.0 |

### Constrained — non-regex group (+ `mux` without constraint)

| scenario | **mux-plain** | httprouter | gin | echo | stdlib | fiber |
|---|---:|---:|---:|---:|---:|---:|
| `[0-9]+` | **28.0** | 40.4 | 48.6 | 49.6 | 101.8 | 97.0 |
| UUID | **29.5** | 54.7 | 60.4 | 44.6 | 184.1 | 126.0 |
| blog | **42.0** | 59.3 | 64.0 | 61.0 | 193.2 | 112.6 |
| slug | **28.5** | 42.9 | 48.4 | 39.7 | 128.3 | 95.4 |

### SIMD

`GOEXPERIMENT=simd` does **not** improve the constrained numbers above for
`mux` (default/SIMD: 38/34, 115/125, 92/109, 52/58 — the spread is host
noise). The realistic constraints validate in **short chunks** (8/4/12 bytes
for a UUID), below the 32-byte vector width, so only the SWAR path runs. SIMD
pays off for a **long single-class parameter** — e.g. a 108-byte slug goes from
83 ns (SWAR) to 44 ns (SIMD); see `BenchmarkValidator` in the main module.

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
| **mux** | **2.0** | **4.0** | **7.0** | 13.0 | **2.0** |
| mux-plain | **2.0** | **4.0** | **7.0** | **3.0** | **2.0** |
| httprouter | 3.0 | 31.5 | 36.5 | 16.0 | 57.7 |
| gin | 5.0 | 6.7 | 8.8 | 8.0 | 7.3 |
| echo | 4.0 | 6.5 | 10.0 | 6.0 | 250.8 |
| bunrouter | 129.7 | 5.0 | 141.5 | 7.0 | 55.2 |
| stdlib | 67.3 | 83.3 | 110.8 | 91.3 | 266.5 |
| compat | 70.7 | 87.5 | 113.8 | 106.2 | 155.5 |
| chi | 131.2 | 244.5 | 275.3 | 300.3 | 182.8 |
| gorilla | 285.0 | 406.3 | 479.0 | 494.7 | 426.7 |
| fiber | 50.3 | 52.2 | 59.2 | 54.0 | 109.7 |

* **`mux` leads every no-constraint case.** It ties `mux-plain` on `static`,
  `param` and `deep`, and is far ahead of the pack on `param` (4.0 vs 5.0 for
  bunrouter) and `miss` (2.0 vs 7.3 for gin). No locks, no per-request
  allocation, so it scales with cores.
* **`compat` inherits the stdlib's ceiling.** It tracks raw `stdlib` (70.7 vs
  67.3 on static, 87.5 vs 83.3 on param) — both are limited by the `ServeMux`'s
  `RWMutex`: the read lock is a shared atomic on one cache line, so it
  ping-pongs across cores. Single-threaded `mux` is ~4x faster than `compat`;
  in parallel it is ~35x, and that gap is the lock, not the routing.
* **httprouter's per-request `Params` slice** costs it under load: its `param`
  goes from 3.0 ns (static) to 31.5 ns, while `mux` (params by value) stays at
  4.0. Allocations scale GC work with cores.
* **gin and echo scale well** (they keep params in a per-request context of
  their own); `fiber` is middling here because the reused `RequestCtx` is per
  goroutine, not per connection pool.

---

## Reading the numbers

* **`mux` (typed) wins every no-constraint scenario** with 0 allocations,
  including the 405 (44.0 vs bunrouter's 46.0).
* **In the regex group `mux` wins by ~5–11x.** The fixed-shape validator
  (UUID, date) is a single linear pass — no backtracking, no allocation — and
  reuses the same byte-class scanner (SWAR/SIMD) as the simple cases.
* **In the non-regex group `mux-plain` leads all four** (28–42 ns, ahead of
  echo and httprouter).
* **The `http.Handler` adapter is expensive**: `httprouter-http` is 178 ns vs
  57 ns for its native handler on `param`. That is precisely why `mux` (typed)
  exists next to `compat` (stdlib semantics).
* **`compat` beats `chi`** (the closest peer, same API style) by ~2.5x on hits,
  but pays for stdlib semantics on 404/405 (body + allowed-methods scan).
* **gorilla** is consistently the slowest; **stdlib** is strong on static but
  expensive on wildcard/404/405.

Caveats: handlers read at most one param; gin/echo are full frameworks (only
their router is measured here); Fiber uses fasthttp; the host is noisy, so
compare ratios from the same run rather than absolute values across runs.
