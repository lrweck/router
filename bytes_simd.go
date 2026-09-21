//go:build goexperiment.simd

package router

import "simd"

// bytesInClass reports whether every byte matches a range or an exact char.
//
// Vectorized with the portable simd package (hardware SIMD on amd64/arm64):
// each alternative contributes a miss vector (clamp+xor for ranges, xor for
// chars), normalized to 0/1 and AND-ed — a lane stays 1 only if it missed
// every alternative — then OR-ed across chunks and reduced once at the end.
//
// Where SIMD can't run — inputs shorter than one vector, a tail shorter than
// a full vector, widths beyond the staging buffer, and pure-Go emulated SIMD
// — the SWAR path (bytes.go) takes over rather than the per-byte loop: it's
// eight bytes per instruction and needs no vectors. SWAR itself falls back to
// the loop for non-ASCII classes, so semantics match the scalar build.
//
// ponytail: one vector width minimum before SIMD pays for itself; ceiling is
// the portable simd API (no unsigned compares, hence the clamp trick).
func bytesInClass(b []byte, ranges [][2]byte, chars []byte) bool {
	n := simd.BroadcastUint8s(0).Len()
	if n > 64 || len(b) < n || simd.Emulated() {
		return swarAllInClass(b, ranges, chars)
	}
	ones := simd.BroadcastUint8s(1)
	var bad simd.Uint8s // zero vector: no bad lanes yet
	i := 0
	for ; i+n <= len(b); i += n {
		v := simd.LoadUint8s(b[i:])
		miss := ones
		for _, r := range ranges {
			e := v.Max(simd.BroadcastUint8s(r[0])).Min(simd.BroadcastUint8s(r[1])).Xor(v)
			miss = miss.And(e.Min(ones))
		}
		for _, c := range chars {
			miss = miss.And(v.Xor(simd.BroadcastUint8s(c)).Min(ones))
		}
		bad = bad.Or(miss)
	}
	var tmp [64]byte
	bad.Store(tmp[:n])
	for _, x := range tmp[:n] {
		if x != 0 {
			return false
		}
	}
	return swarAllInClass(b[i:], ranges, chars)
}
