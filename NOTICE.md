# Attributions and third-party notices

This project was **heavily inspired by, and in places derived from, other
projects**. They are listed here explicitly, with their licenses. The Chi debt
is the largest: the whole public API, the pattern syntax and the behavioral
test suite come from it.

## go-chi/chi — MIT — **API, pattern syntax and ported tests**

The public API (`NewRouter`, `Get`/`Post`/…, `Use`/`With`/`Group`/`Route`/
`Mount`, `URLParam`, `RouteContext`, `Walk`/`Routes`, the `Router` interface in
closures) is a deliberate reimplementation of **Chi**'s, so that Chi code works
unchanged. The `{name}`, `{name:regexp}` and `*` pattern syntax is Chi's.

The behavioral test suite in [`chi_parity_test.go`](chi_parity_test.go) is
**ported/adapted from Chi's `mux_test.go`** (same cases, same expectations,
adjusted where the stdlib backend necessarily differs). Because that file is a
derivative work, Chi's license is reproduced in full below.

- Source: https://github.com/go-chi/chi

```
Copyright (c) 2015-present Peter Kieltyka (https://github.com/pkieltyka), Google Inc.

MIT License

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
the Software, and to permit persons to whom the Software is furnished to do so,
subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
```

## julienschmidt/httprouter — BSD-3-Clause — **typed-handler idea**

The `Mux` engine's central idea — a tree router whose handler receives the path
parameters as arguments, so a match allocates nothing — comes from
**httprouter** (via Gin, which embeds it). No code was copied; the design was
reimplemented. Credited here explicitly.

- Source: https://github.com/julienschmidt/httprouter
- License: BSD 3-Clause.

## ibraheemdev/matchit — MIT — **matching strategy**

The static-first matching and the "compare directly against the path, prioritize
the branch most likely to match" approach of the `Mux` tree were informed by
**matchit** (the Rust router used by axum). No code was copied.

- Source: https://github.com/ibraheemdev/matchit
- License: MIT.

## Go standard library — **behavior and building blocks**

`Compat` is built on `net/http.ServeMux` and deliberately keeps the stdlib's
behavior on every point where it differs from Chi (path canonicalization,
escaping, the `Allow` header format, `HEAD`-from-`GET`). The `nextSlash` and
`dotValidator` fast paths use `strings.IndexByte` (the runtime's SIMD `memchr`).

- Source: https://go.dev/src/net/http/

## Hacker's Delight — **SWAR reference**

The SWAR byte-class scanner in `constraint.go` follows the "SIMD Within A
Register" techniques popularized by **Hacker's Delight** (Henry S. Warren Jr.)
and Daniel Lemire's write-ups. The classic `hasless` propagates borrows between
bytes, which is wrong for combining per-byte masks, so the comparisons here are
carry-free (`swarGE`/`swarLE`/`swarEQ`).

- https://en.wikipedia.org/wiki/Hacker%27s_Delight
- https://lemire.me/blog/2025/04/13/detect-control-characters-quotes-and-backslashes-efficiently-using-swar/
