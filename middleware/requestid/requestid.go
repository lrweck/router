// Package requestid assigns an ID to every request, so log lines and responses
// can be correlated. It uses only the standard library.
//
// There is no RFC for request IDs: X-Request-Id is the de-facto header (an IETF
// draft, draft-ietf-httpapi-request-id, standardizes Request-Id). The value is
// treated as an RFC 9110 token, so a client cannot inject header bytes.
package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// Header is the header carrying the request ID, on the request and the response.
const Header = "X-Request-Id"

type ctxKey struct{}

// FromContext returns the request ID stored in ctx, or "".
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// From returns the request ID of r, or "".
func From(r *http.Request) string { return FromContext(r.Context()) }

// New returns a middleware that ensures every request has an ID: it reuses the
// incoming Header when it is a sane token and generates one otherwise. The ID
// is available through FromContext/From and echoed on the response.
func New() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(Header)
			if !validID(id) {
				id = genID()
			}
			w.Header().Set(Header, id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
		})
	}
}

// validID rejects values that are too long or not a safe header token, so a
// client cannot inject junk (or break the response header) through the ID.
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

func genID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0" // rand failure is not expected; never return an empty ID
	}
	return hex.EncodeToString(b[:])
}
