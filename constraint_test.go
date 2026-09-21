package router

import (
	"bytes"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// TestMatchBudgetIsConservative pins the DoS ceiling's failure mode: a
// budget-exhausted match may under-accept (false negative -> 404) but must
// never over-accept (no false positive). The pathological case is Chi's RE2
// advantage made visible: Chi matches it, we reject it, and that is deliberate
// (constraint.go's matchBudget).
func TestMatchBudgetIsConservative(t *testing.T) {
	type tc struct {
		expr, in string
		// budgeted result under the documented ceiling
		wantBudgeted bool
	}
	pathological := "000000000000000000000000\x80\x80\x80\x80\x8000L0000000000"
	cases := []tc{
		{`(.*(.*())*())0`, pathological, false}, // budget exhausted: rejected
	}
	for _, c := range cases {
		n := parseConstraint(c.expr, "budget")
		budgeted := compileConstraint(c.expr, "budget")(c.in)
		unlimited := genericMatch(n, c.in)
		if budgeted != c.wantBudgeted {
			t.Errorf("%q on %q: budgeted=%v want %v", c.expr, c.in, budgeted, c.wantBudgeted)
		}
		if budgeted && !unlimited {
			t.Errorf("%q on %q: budgeted accepted but unlimited rejected (false positive!)", c.expr, c.in)
		}
	}

	// Realistic generic constraints on large inputs must stay well under the
	// budget: only pathological nesting should ever exhaust it.
	realistic := []string{`[a-z]+`, `[a-z]+-[0-9]+`, `(asc|desc)`, `[0-9]{4}-[0-9]{2}-[0-9]{2}`}
	inputs := []string{
		strings.Repeat("abc-def-123", 900), // ~10KB
		strings.Repeat("9", 10000),
		strings.Repeat("x", 10000),
	}
	for _, expr := range realistic {
		v := compileConstraint(expr, "budget")
		for _, in := range inputs {
			_ = v(in) // must return, not hang; result is expr-dependent
		}
	}
}

// FuzzConstraintFastVsGeneric hammers the fast validators (SWAR class loop,
// literal alternation) against the generic backtracker, the reference for
// "does this constraint accept this value". Only constraints that take a fast
// path are fuzzed: for everything else compileConstraint *is* the generic
// matcher, and the only possible divergence is the matchBudget ceiling
// (documented in constraint.go), not a matching bug.
func FuzzConstraintFastVsGeneric(f *testing.F) {
	exprs := []string{
		`[0-9]+`, `\d+`, `\d{4}`, `[0-9]{2,4}`, `[a-z]+`, `[a-z0-9-]+`,
		`\w+`, `.+`, `[^/]+`, `[A-Z]{2}`, `(ab|cd)+`, `(asc|desc)`, `[a-z]+-[0-9]+`,
		`[0-9]{4}-[0-9]{2}-[0-9]{2}`, `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`,
	}
	for _, e := range exprs {
		f.Add(e, "2026")
		f.Add(e, "abc-def-123")
		f.Add(e, "")
	}
	f.Fuzz(func(t *testing.T, expr, in string) {
		n, ok := tryParse(expr)
		if !ok || fastValidator(n) == nil {
			return // invalid, or generic-only (same matcher as the reference)
		}
		if got, want := compileConstraint(expr, "fuzz")(in), genericMatch(n, in); got != want {
			t.Fatalf("expr %q input %q: fast=%v generic=%v", expr, in, got, want)
		}
	})
}

// tryParse parses expr, reporting ok=false for patterns the parser rejects.
func tryParse(expr string) (n cnode, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	if expr == "" {
		return nil, false
	}
	return parseConstraint(expr, "fuzz"), true
}

// genericMatch is the reference: full-match against the backtracker with no
// budget (unlike production), so it can't mask a fast-path bug.
func genericMatch(n cnode, in string) bool {
	return slices.Contains(n.match(in, 0, nil), len(in))
}

// TestFastValidatorAgrees is a structured differential test: the fast
// validator (SWAR/class loop, literal alternation) must agree with the
// generic backtracker on every input.
func TestFastValidatorAgrees(t *testing.T) {
	exprs := []string{
		`[0-9]+`, `\d+`, `\d{4}`, `[0-9]{2,4}`, `[a-z]+`, `[a-z0-9-]+`,
		`\w+`, `.+`, `[^/]+`, `[A-Z]{2}`, `(ab|cd)+`, `(a|ab)+`, `(asc|desc)`,
		// fixed-shape sequences (fastSeq): UUID, date, version, literal
		`[0-9]{4}-[0-9]{2}-[0-9]{2}`,
		`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`,
		`\d{4}-\d{2}`,
		`[a-z]{3}\.txt`,
		`abc`,
		`v[0-9]{1,3}\.[0-9]{1,3}`,
	}
	inputs := []string{
		"", "a", "0", "2026", "123456789012345",
		strings.Repeat("1234567890", 12) + "12345678", // 128B
		strings.Repeat("abc-def-0123456789", 6),       // 108B multi-chunk
		"abc-def", "ABC", "ab", "cd", "abcd", "abx", "aab", "aaab",
		strings.Repeat("ab", 40), strings.Repeat("abcd", 20),
		".", "/", "a/b", "9:", "`a", "a{", "z", "{",
		"12a", "a12", "\xff\x80ab", "caf\xc3\xa9", // high-bit bytes
		strings.Repeat("x", 31), strings.Repeat("x", 32), strings.Repeat("x", 33),
		strings.Repeat("7", 63), strings.Repeat("7", 64), strings.Repeat("7", 65),
		"2026-09-21", "2026-9-21", "550e8400-e29b-41d4-a716-446655440000",
		"550e8400-e29b-41d4-a716-44665544000", "abc.txt", "abcd.txt", "abc", "ab",
		"v1.2", "v1.2.3.4", "v10.200",
	}
	for _, expr := range exprs {
		n := parseConstraint(expr, "test")
		fv := fastValidator(n)
		if fv == nil {
			t.Logf("%s: generic path (no fast form)", expr)
		}
		for _, in := range inputs {
			want := slices.Contains(n.match(in, 0, nil), len(in))
			got := compileConstraint(expr, "test")(in)
			if got != want {
				t.Errorf("expr %q input %q: fast=%v generic=%v", expr, in, got, want)
			}
		}
	}
}

// TestBytesInClassDifferential pins the hand-written SWAR matcher to the
// plain per-byte reference on structured inputs (all single bytes, lengths
// across the 8-byte word boundary, high-bit bytes, empty).
func TestBytesInClassDifferential(t *testing.T) {
	classes := []struct {
		name   string
		ranges [][2]byte
		chars  []byte
	}{
		{"digits", [][2]byte{{'0', '9'}}, nil},
		{"slug", [][2]byte{{'a', 'z'}, {'0', '9'}}, []byte{'-'}},
		{"upper", [][2]byte{{'A', 'Z'}}, nil},
		{"word", [][2]byte{{'A', 'Z'}, {'a', 'z'}, {'0', '9'}}, []byte{'_'}},
		{"chars", nil, []byte{'a', 'b'}},
		{"allascii", [][2]byte{{0, 127}}, nil},
		{"nul-nl", [][2]byte{{0, 0}, {9, 9}, {10, 10}}, nil},
		{"highclass", [][2]byte{{0x80, 0xff}}, nil}, // non-ASCII: scalar fallback
	}
	inputs := [][]byte{nil, {}, {0}, {127}, {128}, {255}}
	for b := range 256 {
		inputs = append(inputs, []byte{byte(b)})
		for _, n := range []int{7, 8, 9, 15, 16, 17, 31, 33} {
			inputs = append(inputs, bytes.Repeat([]byte{byte(b)}, n))
		}
	}
	inputs = append(inputs,
		[]byte("123456789012345"),
		bytes.Repeat([]byte("abc-def-0123456789"), 6),
		[]byte("caf\xc3\xa9"),
		[]byte("\xff\x80ab"),
		[]byte("1234\x006789"),
	)
	for _, cl := range classes {
		for _, in := range inputs {
			want := bytesInClassLoop(in, cl.ranges, cl.chars)
			if got := swarAllInClass(in, cl.ranges, cl.chars); got != want {
				t.Fatalf("%s %q: swar=%v loop=%v", cl.name, in, got, want)
			}
			// The build-selected matcher (SWAR, or SIMD+SWAR tail under
			// GOEXPERIMENT=simd) must agree with the reference too.
			if got := bytesInClass(in, cl.ranges, cl.chars); got != want {
				t.Fatalf("%s %q: bytesInClass=%v loop=%v", cl.name, in, got, want)
			}
		}
	}
}

// FuzzBytesInClass hammers the SWAR matcher against the reference with
// adversarial byte patterns (borrow/carry boundaries, word crossings).
func FuzzBytesInClass(f *testing.F) {
	f.Add([]byte("123456789012345"), uint8(0))
	f.Add(bytes.Repeat([]byte("ab-"), 20), uint8(1))
	f.Add([]byte("\xff\x80/xyz"), uint8(2))
	f.Add([]byte{}, uint8(3))
	f.Fuzz(func(t *testing.T, in []byte, classSel uint8) {
		classes := []struct {
			ranges [][2]byte
			chars  []byte
		}{
			{[][2]byte{{'0', '9'}}, nil},
			{[][2]byte{{'a', 'z'}, {'0', '9'}}, []byte{'-'}},
			{[][2]byte{{0, 127}}, nil},
			{nil, []byte{'/', 0}},
		}
		cl := classes[int(classSel)%len(classes)]
		want := bytesInClassLoop(in, cl.ranges, cl.chars)
		if got := swarAllInClass(in, cl.ranges, cl.chars); got != want {
			t.Fatalf("in=%q ranges=%v chars=%v: swar=%v loop=%v", in, cl.ranges, cl.chars, got, want)
		}
		if got := bytesInClass(in, cl.ranges, cl.chars); got != want {
			t.Fatalf("in=%q ranges=%v chars=%v: bytesInClass=%v loop=%v", in, cl.ranges, cl.chars, got, want)
		}
	})
}

// TestSwarHelpersExhaustive checks the carry-free comparison primitives
// directly against per-byte arithmetic, including the borrow-prone cases.
func TestSwarHelpersExhaustive(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for range 200000 {
		var x uint64
		for i := range 8 {
			x |= uint64(rng.Intn(128)) << (8 * i) // ASCII word
		}
		for n := 0; n <= 128; n++ {
			ge, le := swarGE(x, n), swarLE(x, n)
			for i := range 8 {
				b := byte(x >> (8 * i))
				if (ge>>(8*i))&0x80 != 0 != (int(b) >= n) {
					t.Fatalf("GE x=%016x n=%d byte=%d", x, n, b)
				}
				if n <= 127 {
					if (le>>(8*i))&0x80 != 0 != (int(b) <= n) {
						t.Fatalf("LE x=%016x n=%d byte=%d", x, n, b)
					}
				}
			}
		}
		for c := range 128 {
			eq := swarEQ(x, byte(c))
			for i := range 8 {
				b := byte(x >> (8 * i))
				if (eq>>(8*i))&0x80 != 0 != (b == byte(c)) {
					t.Fatalf("EQ x=%016x c=%d byte=%d", x, c, b)
				}
			}
		}
	}
}
