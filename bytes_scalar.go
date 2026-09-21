//go:build !goexperiment.simd

package router

// bytesInClass reports whether every byte matches a range or an exact char.
// Scalar build: SWAR, eight bytes per iteration (see bytes.go), with the
// per-byte loop for tails and non-ASCII classes. (With GOEXPERIMENT=simd the
// vectorized version in bytes_simd.go is used instead; same signature.)
func bytesInClass(b []byte, ranges [][2]byte, chars []byte) bool {
	return swarAllInClass(b, ranges, chars)
}
