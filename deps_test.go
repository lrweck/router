package router

import (
	"os"
	"strings"
	"testing"
)

// TestStdlibOnly guards the zero-dependency promise: the module must not
// require anything outside the standard library. A middleware that needs an
// external library belongs in its own module (see middleware/README.md).
func TestStdlibOnly(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	inRequire := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "//"):
		case strings.HasPrefix(line, "require ("):
			inRequire = true
		case inRequire && line == ")":
			inRequire = false
		case inRequire, strings.HasPrefix(line, "require "):
			t.Errorf("go.mod requires %q; keep the module stdlib-only", line)
		}
	}
}
