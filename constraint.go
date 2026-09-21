// Constraint validation without the regexp package.
//
// Chi supports {name:regex} params with full RE2 syntax (github.com/go-chi/chi,
// MIT). regexp is slow and heavy for route matching, so this file compiles the
// common subset of constraints into a small backtracking matcher over plain
// predicates:
//
//   - character classes: [0-9], [a-z-], [^...], \d \D \w \W \s \S
//   - literals and escapes: \. \- \\ \t \n \r ...
//   - "." (any char except '/'), ^...$ anchors (implicit full match anyway)
//   - quantifiers: ? * + {n} {n,} {n,m}
//   - groups with alternation: (asc|desc), (?:foo|bar)
//
// Anything else (\p{...}, (?i), backreferences, ...) panics at registration
// with a clear message, like Chi panics on invalid patterns. That is the
// deliberate ceiling: rewrite the constraint with the constructs above.
// A '/' can never match (params never contain one); it panics, except inside
// negated classes ([^/] means "any char", the idiom for a plain param).
package router

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
	"unsafe"
)

// validator reports whether v fully matches a compiled constraint.
type validator func(v string) bool

// compileConstraint parses expr (the part after ':' in {name:expr}) into a
// validator. Empty expr means unconstrained (nil). The most common shape —
// one repeated byte class — takes the allocation-free fast path (below);
// everything else uses the generic backtracker.
func compileConstraint(expr, pattern string) validator {
	if expr == "" {
		return nil
	}
	n := parseConstraint(expr, pattern)
	if fv := fastValidator(n); fv != nil {
		return fv
	}
	return func(v string) bool {
		m := &matcher{left: matchBudget}
		return slices.Contains(n.match(v, 0, m), len(v))
	}
}

// u8 reinterprets a string as bytes without copying (read-only use).
func u8(s string) []byte {
	if s == "" {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// fastValidator compiles one repeated byte class (digits, slugs, fixed
// widths: the overwhelmingly common route constraint) into a tight loop over
// bytesInClass, which the toolchain vectorizes when GOEXPERIMENT=simd is set
// (see bytes_*.go) and runs scalar otherwise. Returns nil when the shape
// doesn't fit, falling back to the generic backtracker.
//
// ponytail: single-class fast path only; multi-class/alternation stays on the
// backtracker until a profile says otherwise.
func fastValidator(n cnode) validator {
	var cls clsNode
	var min, max int
	switch t := n.(type) {
	case repNode:
		switch inner := t.n.(type) {
		case clsNode:
			cls, min, max = inner, t.min, t.max
		case dotNode:
			return dotValidator(t.min, t.max)
		case altNode:
			if opts, ok := litOptionsOf(inner); ok {
				return litAltValidator(opts, t.min, t.max)
			}
			return nil
		default:
			return nil
		}
	case clsNode:
		cls, min, max = t, 1, 1
	case dotNode:
		return dotValidator(1, 1)
	case altNode:
		if opts, ok := litOptionsOf(t); ok {
			return litAltValidator(opts, 1, 1)
		}
		return nil
	case seqNode:
		return fastSeq(t)
	case litNode:
		lit := string(t)
		return func(v string) bool { return v == lit }
	default:
		return nil
	}
	ranges, chars, ok := classBytes(cls)
	if !ok {
		return nil
	}
	return func(v string) bool {
		if len(v) < min {
			return false
		}
		if max >= 0 && len(v) > max {
			return false
		}
		return bytesInClass(u8(v), ranges, chars)
	}
}

// classBytes flattens a class into ranges + exact chars, resolving symbolic
// escapes (\d, \w, \s). Returns ok=false for negated classes or \D/\W/\S.
func classBytes(cls clsNode) (ranges [][2]byte, chars []byte, ok bool) {
	if cls.neg {
		return nil, nil, false
	}
	ranges = append([][2]byte{}, cls.ranges...)
	chars = append([]byte{}, cls.chars...)
	for _, c := range cls.classes {
		switch c {
		case 'd':
			ranges = append(ranges, [2]byte{'0', '9'})
		case 'w':
			ranges = append(ranges, [2]byte{'0', '9'}, [2]byte{'a', 'z'}, [2]byte{'A', 'Z'})
			chars = append(chars, '_')
		case 's':
			chars = append(chars, ' ', '\t', '\n', '\r', '\f', '\v')
		default:
			return nil, nil, false // \D \W \S: generic path
		}
	}
	if len(ranges) == 0 && len(chars) == 0 {
		return nil, nil, false
	}
	return ranges, chars, true
}

// seqStep is one element of a fixed-shape constraint.
type seqStep struct {
	lit    string
	ranges [][2]byte
	chars  []byte
	dot    bool
	n      int
}

// fastSeq compiles a concatenation of literals and fixed-count classes/dots
// (UUID, dates, versions, IPs: the realistic route constraints) into a single
// linear pass — no backtracking, no allocation, and the class checks go through
// bytesInClass (SWAR/SIMD). Returns nil for variable counts, alternation, etc.
func fastSeq(seq seqNode) validator {
	steps := make([]seqStep, 0, len(seq))
	for _, c := range seq {
		switch t := c.(type) {
		case litNode:
			steps = append(steps, seqStep{lit: string(t)})
		case clsNode:
			r, ch, ok := classBytes(t)
			if !ok {
				return nil
			}
			steps = append(steps, seqStep{ranges: r, chars: ch, n: 1})
		case dotNode:
			steps = append(steps, seqStep{dot: true, n: 1})
		case repNode:
			if t.min != t.max { // fixed count only
				return nil
			}
			switch inner := t.n.(type) {
			case clsNode:
				r, ch, ok := classBytes(inner)
				if !ok {
					return nil
				}
				steps = append(steps, seqStep{ranges: r, chars: ch, n: t.min})
			case dotNode:
				steps = append(steps, seqStep{dot: true, n: t.min})
			default:
				return nil
			}
		default:
			return nil
		}
	}
	return func(v string) bool {
		i := 0
		for _, st := range steps {
			if st.lit != "" {
				if !strings.HasPrefix(v[i:], st.lit) {
					return false
				}
				i += len(st.lit)
				continue
			}
			if len(v)-i < st.n {
				return false
			}
			seg := v[i : i+st.n]
			if st.dot {
				if strings.IndexByte(seg, '/') >= 0 {
					return false
				}
			} else if !bytesInClass(u8(seg), st.ranges, st.chars) {
				return false
			}
			i += st.n
		}
		return i == len(v)
	}
}

// litOptionsOf flattens an alternation of pure literals (e.g. asc|desc) into
// its options. Non-literal or empty branches bail to the generic path
// (empties would risk zero-width loops).
func litOptionsOf(a altNode) ([]string, bool) {
	opts := make([]string, 0, len(a))
	for _, b := range a {
		var sb strings.Builder
		nodes := []cnode{b}
		if seq, ok := b.(seqNode); ok {
			nodes = seq
		}
		for _, c := range nodes {
			l, ok := c.(litNode)
			if !ok {
				return nil, false
			}
			sb.WriteString(string(l))
		}
		if sb.Len() == 0 {
			return nil, false
		}
		opts = append(opts, sb.String())
	}
	return opts, true
}

type litFrame struct {
	pos, next, cnt int
}

// litAltValidator matches a repetition of literal options with explicit
// backtracking: zero-alloc for shallow stacks (heap spill past 32 frames,
// still correct). Covers Chi's (asc|desc)-style constraints.
func litAltValidator(opts []string, min, max int) validator {
	minOpt, maxOpt := len(opts[0]), 0
	for _, o := range opts {
		if len(o) < minOpt {
			minOpt = len(o)
		}
		if len(o) > maxOpt {
			maxOpt = len(o)
		}
	}
	return func(v string) bool {
		if len(v) < min*minOpt {
			return false
		}
		if max >= 0 && len(v) > max*maxOpt {
			return false
		}
		budget := matchBudget // backtracking can revisit positions; cap total steps
		var buf [32]litFrame
		stack := buf[:0]
		pos, cnt, next := 0, 0, 0
		for {
			budget--
			if budget < 0 {
				return false
			}
			if cnt >= min && pos == len(v) {
				return true
			}
			if cnt != max {
				if k, ok := matchOptAt(v, pos, opts, next); ok {
					stack = append(stack, litFrame{pos, k + 1, cnt})
					pos += len(opts[k])
					cnt++
					next = 0
					continue
				}
			}
			if len(stack) == 0 {
				return false
			}
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			pos, next, cnt = f.pos, f.next, f.cnt
		}
	}
}

func matchOptAt(s string, pos int, opts []string, from int) (int, bool) {
	for k := from; k < len(opts); k++ {
		if strings.HasPrefix(s[pos:], opts[k]) {
			return k, true
		}
	}
	return 0, false
}

// dotValidator checks "any run of non-'/' bytes" with the runtime memchr
// (already SIMD in stdlib): no experimental toolchain needed.
func dotValidator(min, max int) validator {
	return func(v string) bool {
		if len(v) < min {
			return false
		}
		if max >= 0 && len(v) > max {
			return false
		}
		return strings.IndexByte(v, '/') < 0
	}
}

// bytesInClassLoop is the shared scalar core: every byte must match a range
// or an exact char. Used directly in scalar builds and for SIMD tails.
func bytesInClassLoop(b []byte, ranges [][2]byte, chars []byte) bool {
	for _, c := range b {
		if !byteInClass(c, ranges, chars) {
			return false
		}
	}
	return true
}

func byteInClass(c byte, ranges [][2]byte, chars []byte) bool {
	for _, r := range ranges {
		if r[0] <= c && c <= r[1] {
			return true
		}
	}
	return slices.Contains(chars, c)
}

// matchBudget caps elementary steps per match call. Hand-written matchers
// have no RE2-style linear guarantee (which Chi gets for free), so this is
// the ceiling that keeps a pathological constraint from hanging the server:
// exhaustion fails the match (→404) instead of burning CPU. Normal matches
// use hundreds of steps; the budget allows a million.
const matchBudget = 1 << 20

// matcher threads a work budget through generic matching. nil means
// unlimited (tests only — production paths always pass a real budget).
type matcher struct{ left int }

func (m *matcher) take(n int) bool {
	if m == nil {
		return true
	}
	m.left -= n
	return m.left >= 0
}

// cnode is one matcher node. match returns all reachable end positions.
type cnode interface {
	match(s string, i int, m *matcher) []int
}

type seqNode []cnode
type altNode []cnode
type repNode struct {
	n        cnode
	min, max int // max < 0 means unbounded
}
type litNode string
type dotNode struct{}
type clsNode struct {
	neg     bool
	ranges  [][2]byte
	chars   []byte
	preds   []func(byte) bool
	classes []byte // symbolic class escapes used (\d \w ...), for the fast path
}

// uniqEnds compacts reachable end positions in place. Match results are only
// ever tested for membership, so duplicates are pure waste — and without
// this, nested repetitions (e.g. `(a+)+`) blow up exponentially like a naive
// backtracker. Chi uses RE2 (linear-time); this keeps us polynomial.
func uniqEnds(out []int) []int {
	if len(out) < 2 {
		return out
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (n seqNode) match(s string, i int, m *matcher) []int {
	if !m.take(1) {
		return nil
	}
	cur := []int{i}
	for _, c := range n {
		var nxt []int
		for _, p := range cur {
			ends := c.match(s, p, m)
			if !m.take(len(ends)) {
				return nil
			}
			nxt = append(nxt, ends...)
		}
		cur = uniqEnds(nxt)
		if len(cur) == 0 {
			break
		}
	}
	return cur
}

func (n altNode) match(s string, i int, m *matcher) []int {
	if !m.take(1) {
		return nil
	}
	var out []int
	for _, b := range n {
		ends := b.match(s, i, m)
		if !m.take(len(ends)) {
			return nil
		}
		out = append(out, ends...)
	}
	return uniqEnds(out)
}

func (n repNode) match(s string, i int, m *matcher) []int {
	// Level-by-level expansion over exact rep counts: frontier[c] holds the
	// positions reachable in exactly c reps, recorded when c is in
	// [min, max]. Re-expansion across levels is allowed (no cross-level
	// visited set), which is what captures paths that reach the same end
	// with more reps (e.g. `(aa|a){3}` on "aaaa" via a+a+aa — a
	// first-seen-only scheme misses it). Positions only move forward, so
	// levels terminate; per-level dedup keeps nested reps polynomial.
	if i > len(s) {
		return nil
	}
	if !m.take(1) {
		return nil
	}
	var out []int
	recorded := make([]bool, len(s)+1)
	frontier := []int{i}
	for cnt := 0; ; cnt++ {
		frontier = uniqEnds(frontier)
		if cnt >= n.min {
			for _, p := range frontier {
				if !m.take(1) {
					return nil
				}
				if !recorded[p] {
					recorded[p] = true
					out = append(out, p)
				}
			}
		}
		if n.max >= 0 && cnt >= n.max {
			break
		}
		var next []int
		for _, p := range frontier {
			for _, e := range n.n.match(s, p, m) {
				if e == p || e > len(s) {
					continue // zero-width or out of range: no progress
				}
				if !m.take(1) {
					return nil
				}
				next = append(next, e)
			}
		}
		if len(next) == 0 {
			break
		}
		frontier = next
	}
	return out
}

func (n litNode) match(s string, i int, m *matcher) []int {
	if !m.take(1) {
		return nil
	}
	if strings.HasPrefix(s[i:], string(n)) {
		return []int{i + len(n)}
	}
	return nil
}

func (dotNode) match(s string, i int, m *matcher) []int {
	if !m.take(1) {
		return nil
	}
	if i < len(s) && s[i] != '/' {
		return []int{i + 1}
	}
	return nil
}

func (n clsNode) match(s string, i int, m *matcher) []int {
	if !m.take(1) {
		return nil
	}
	if i >= len(s) {
		return nil
	}
	c := s[i]
	hit := false
	for _, r := range n.ranges {
		if r[0] <= c && c <= r[1] {
			hit = true
			break
		}
	}
	if !hit {
		if slices.Contains(n.chars, c) {
			hit = true
		}
	}
	if !hit {
		for _, p := range n.preds {
			if p(c) {
				hit = true
				break
			}
		}
	}
	if n.neg != hit {
		return []int{i + 1}
	}
	return nil
}

type cparser struct {
	s       string
	pos     int
	pattern string
}

func (p *cparser) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	panic(fmt.Sprintf("router: invalid constraint in pattern %q: %s", p.pattern, msg))
}

func parseConstraint(expr, pattern string) cnode {
	p := &cparser{s: expr, pattern: pattern}
	p.s = strings.TrimPrefix(p.s, "^")
	p.s = strings.TrimSuffix(p.s, "$")
	n := p.parseAlt()
	if p.pos != len(p.s) {
		p.fail("unexpected %q", p.s[p.pos:])
	}
	return n
}

func (p *cparser) peek() byte {
	if p.pos >= len(p.s) {
		return 0
	}
	return p.s[p.pos]
}

func (p *cparser) parseAlt() cnode {
	var branches []cnode
	branches = append(branches, p.parseSeq())
	for p.peek() == '|' {
		p.pos++
		branches = append(branches, p.parseSeq())
	}
	if len(branches) == 1 {
		return branches[0]
	}
	return altNode(branches)
}

func (p *cparser) parseSeq() cnode {
	var nodes []cnode
	for {
		c := p.peek()
		if c == 0 || c == ')' || c == '|' {
			break
		}
		nodes = append(nodes, p.parseAtom())
	}
	if len(nodes) == 1 {
		return nodes[0]
	}
	return seqNode(nodes)
}

func (p *cparser) parseAtom() cnode {
	n := p.parseSingle()
	switch p.peek() {
	case '?':
		p.pos++
		return repNode{n, 0, 1}
	case '*':
		p.pos++
		return repNode{n, 0, -1}
	case '+':
		p.pos++
		return repNode{n, 1, -1}
	case '{':
		mm := p.parseCount()
		return repNode{n, mm[0], mm[1]}
	}
	return n
}

func (p *cparser) parseCount() [2]int {
	save := p.pos
	p.pos++ // consume '{'
	num := func() int {
		start := p.pos
		for p.peek() >= '0' && p.peek() <= '9' {
			p.pos++
		}
		if start == p.pos {
			p.pos = save
			p.fail("invalid quantifier, want {n}, {n,} or {n,m}")
		}
		n := 0
		for _, d := range p.s[start:p.pos] {
			n = n*10 + int(d-'0')
			if n > 65535 {
				p.pos = save
				p.fail("quantifier too large")
			}
		}
		return n
	}
	min := num()
	max := min
	if p.peek() == ',' {
		p.pos++
		max = -1
		if p.peek() != '}' {
			max = num()
			if max < min {
				p.pos = save
				p.fail("quantifier {n,m} with m < n")
			}
		}
	}
	if p.peek() != '}' {
		p.pos = save
		p.fail("invalid quantifier, want {n}, {n,} or {n,m}")
	}
	p.pos++
	_ = save
	return [2]int{min, max}
}

func (p *cparser) parseSingle() cnode {
	c := p.peek()
	switch c {
	case '(':
		if strings.HasPrefix(p.s[p.pos:], "(?:") {
			p.pos += 3
		} else if strings.HasPrefix(p.s[p.pos:], "(?") {
			p.fail("unsupported group flag, only (?:...) is allowed")
		} else {
			p.pos++
		}
		n := p.parseAlt()
		if p.peek() != ')' {
			p.fail("unclosed group")
		}
		p.pos++
		return n
	case '[':
		return p.parseClass()
	case '\\':
		return p.parseEscape(false)
	case '.':
		p.pos++
		return dotNode{}
	case '/':
		p.fail("constraint can never match '/', params stop there")
	case '^', '$':
		p.fail("anchors are only allowed at the start/end, match is already full")
	case '*', '+', '?':
		// Like RE2: a quantifier with nothing to repeat is a literal.
		p.pos++
		return litNode(string([]byte{c}))
	default:
		_, size := utf8.DecodeRuneInString(p.s[p.pos:])
		lit := p.s[p.pos : p.pos+size]
		p.pos += size
		return litNode(lit)
	}
	return litNode("")
}

func classPred(c byte) func(byte) bool {
	switch c {
	case 'd':
		return func(b byte) bool { return '0' <= b && b <= '9' }
	case 'D':
		return func(b byte) bool { return b < '0' || b > '9' }
	case 'w':
		return func(b byte) bool {
			return '0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || b == '_'
		}
	case 'W':
		return func(b byte) bool {
			return !('0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || b == '_')
		}
	case 's':
		return func(b byte) bool {
			return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == '\v'
		}
	case 'S':
		return func(b byte) bool {
			return !(b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == '\v')
		}
	}
	return nil
}

// parseEscape parses one \X escape. inClass reports if inside [...].
func (p *cparser) parseEscape(inClass bool) cnode {
	p.pos++ // consume '\'
	c := p.peek()
	if c == 0 {
		p.fail("trailing backslash")
	}
	if pred := classPred(c); pred != nil {
		p.pos++
		return clsNode{preds: []func(byte) bool{pred}, classes: []byte{c}}
	}
	switch c {
	case 'n':
		p.pos++
		return litNode("\n")
	case 't':
		p.pos++
		return litNode("\t")
	case 'r':
		p.pos++
		return litNode("\r")
	}
	switch c {
	case '.', '*', '+', '?', '(', ')', '[', ']', '{', '}', '|', '^', '$', '\\', '-', '/':
		p.pos++
		return litNode(string([]byte{c}))
	}
	p.fail("unsupported escape \\%c, use a literal or a simple class instead", c)
	return litNode("")
}

func (p *cparser) parseClass() cnode {
	p.pos++ // consume '['
	n := clsNode{}
	if p.peek() == '^' {
		n.neg = true
		p.pos++
	}
	if p.peek() == ']' {
		n.chars = append(n.chars, ']')
		p.pos++
	}
	var closed bool
	for {
		c := p.peek()
		if c == 0 {
			p.fail("unclosed character class")
		}
		if c == ']' {
			p.pos++
			closed = true
			break
		}
		if c == '[' {
			p.fail("nested classes like [[:alpha:]] are not supported")
		}
		if c == '/' {
			// [^/] is the idiom for "any char": '/' can never occur, drop it.
			// A bare '/' elsewhere can never match, fail loudly.
			if !n.neg {
				p.fail("constraint can never match '/'")
			}
			p.pos++
			continue
		}
		var lo byte
		var loPred func(byte) bool
		var loIsPred bool
		if c == '\\' {
			if p.pos+1 < len(p.s) && p.s[p.pos+1] == '/' {
				if !n.neg {
					p.fail("constraint can never match '/'")
				}
				p.pos += 2
				continue
			}
			esc := p.parseEscape(true)
			if ln, ok := esc.(litNode); ok && len(ln) == 1 {
				lo = ln[0]
			} else if cn, ok := esc.(clsNode); ok && len(cn.preds) == 1 {
				loPred, loIsPred = cn.preds[0], true
				n.classes = append(n.classes, cn.classes...)
			} else {
				p.fail("unexpected escape in class")
			}
		} else {
			_, size := utf8.DecodeRuneInString(p.s[p.pos:])
			if size > 1 {
				lit := p.s[p.pos : p.pos+size]
				p.pos += size
				// Multi-byte literal inside a class: keep bytes, matched byte-wise.
				for i := 0; i < len(lit); i++ {
					n.chars = append(n.chars, lit[i])
				}
				continue
			}
			lo = c
			p.pos++
		}
		if !loIsPred && p.peek() == '-' && p.pos+1 < len(p.s) && p.s[p.pos+1] != ']' {
			p.pos++ // consume '-'
			hc := p.peek()
			var hi byte
			if hc == '\\' {
				esc := p.parseEscape(true)
				ln, ok := esc.(litNode)
				if !ok || len(ln) != 1 {
					p.fail("class escape cannot be a range bound")
				}
				hi = ln[0]
			} else {
				_, size := utf8.DecodeRuneInString(p.s[p.pos:])
				if size > 1 {
					p.fail("multi-byte range bounds are not supported")
				}
				hi = hc
				p.pos++
			}
			if hi < lo {
				p.fail("range %c-%c is empty", lo, hi)
			}
			n.ranges = append(n.ranges, [2]byte{lo, hi})
			continue
		}
		if loIsPred {
			n.preds = append(n.preds, loPred)
			continue
		}
		n.chars = append(n.chars, lo)
	}
	if !closed {
		p.fail("unclosed character class")
	}
	if n.neg && len(n.ranges) == 0 && len(n.chars) == 0 && len(n.preds) == 0 {
		// [^/]-style: '/' was dropped above, leaving an empty negation that
		// matches any byte. Normalize to dot: validated values never contain
		// '/' (mux placeholders and '/'-split segments, see matchPath), so
		// this is exact on every real input, and both matcher paths agree.
		return dotNode{}
	}
	return n
}

// SWAR (SIMD Within A Register) byte-class checks: classify 8 bytes per
// iteration with plain uint64 arithmetic, no SIMD toolchain required. The
// technique is from Hacker's Delight (Henry S. Warren Jr.) and Daniel Lemire's
// SWAR write-ups; see NOTICE.md.
//
// Deliberately carry-free: the classic hasless/hasmore propagate borrows
// between bytes, which is fine for an "is there any" test but wrong when masks
// are AND-ed/OR-ed across ranges. The comparisons here add a constant and read
// bit 7 instead (both addends < 128, and input high bits are rejected up
// front), so they compose correctly.
const (
	swarOnes = 0x0101010101010101
	swarHigh = 0x8080808080808080
)

// swarGE returns bit 7 set in each byte where that byte >= n (n in 0..128),
// assuming every input byte is < 128. The addend is 128-n < 128, so the
// per-byte sums can't carry into a neighbour.
func swarGE(x uint64, n int) uint64 {
	if n <= 0 {
		return swarHigh
	}
	return (x + swarOnes*(uint64(128-n)&0x7f)) & swarHigh
}

// swarLE returns bit 7 set in each byte where that byte <= n (n in 0..127).
func swarLE(x uint64, n int) uint64 {
	return ^swarGE(x, n+1) & swarHigh
}

// swarEQ returns bit 7 set in each byte where that byte == c (c < 128).
func swarEQ(x uint64, c byte) uint64 {
	d := x ^ (swarOnes * uint64(c))
	return ^(d + swarOnes*0x7f) & swarHigh
}

// swarBad returns bit 7 set in each byte that matches no range and no char.
// A byte ≥128 is bad for an ASCII-only class, reported as all-bad.
func swarBad(x uint64, ranges [][2]byte, chars []byte) uint64 {
	if x&swarHigh != 0 {
		return swarHigh
	}
	bad := uint64(swarHigh)
	for _, r := range ranges {
		outside := (^swarGE(x, int(r[0])) | swarGE(x, int(r[1])+1)) & swarHigh
		bad &= outside
	}
	for _, c := range chars {
		bad &= ^swarEQ(x, c) & swarHigh
	}
	return bad
}

// asciiClass reports whether every member of the class is < 128, the
// precondition for the SWAR path.
func asciiClass(ranges [][2]byte, chars []byte) bool {
	for _, r := range ranges {
		if r[1] >= 128 {
			return false
		}
	}
	for _, c := range chars {
		if c >= 128 {
			return false
		}
	}
	return true
}

// swarAllInClass is bytesInClassLoop's SWAR twin: eight bytes per iteration,
// scalar loop for the tail and for non-ASCII classes.
//
// ponytail: fewer than one full word goes straight to the scalar loop (the
// setup would cost more than it saves); at 8+ bytes SWAR measured faster even
// for one word. Re-tune the threshold with BenchmarkBytesInClass if the
// average param length changes.
func swarAllInClass(b []byte, ranges [][2]byte, chars []byte) bool {
	if len(b) < 8 || !asciiClass(ranges, chars) {
		return bytesInClassLoop(b, ranges, chars)
	}
	i := 0
	for ; i+8 <= len(b); i += 8 {
		if swarBad(binary.LittleEndian.Uint64(b[i:]), ranges, chars) != 0 {
			return false
		}
	}
	return bytesInClassLoop(b[i:], ranges, chars)
}
