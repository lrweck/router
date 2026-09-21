// Package basicauth guards handlers with HTTP Basic authentication.
//
// The scheme is RFC 7617; the 401 challenge follows RFC 9110 §11.6.1 (the realm
// is a quoted-string) and the server reads credentials with the standard
// Request.BasicAuth, which decodes the RFC 7617 user-pass.
package basicauth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Config configures the middleware. Provide Validator, or Users, or both
// (Validator wins). Validator is the hook for hashed passwords (bcrypt and
// friends) without adding a dependency here.
type Config struct {
	Realm     string
	Users     map[string]string // username -> password
	Validator func(user, password string) bool
}

// New returns a middleware that requires HTTP Basic auth and answers 401 with a
// WWW-Authenticate challenge otherwise.
func New(cfg Config) func(http.Handler) http.Handler {
	realm := cleanRealm(cfg.Realm)
	if realm == "" {
		realm = "Restricted"
	}
	check := cfg.Validator
	if check == nil {
		users := cfg.Users
		check = func(user, pass string) bool {
			want, ok := users[user]
			if !ok {
				return false
			}
			return subtle.ConstantTimeCompare([]byte(pass), []byte(want)) == 1
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			if !ok || !check(user, pass) {
				w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`"`)
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// cleanRealm drops the bytes that would break out of the quoted realm value.
func cleanRealm(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 {
			return -1
		}
		return r
	}, s)
}
