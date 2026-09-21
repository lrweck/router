// Package realip rewrites the request's RemoteAddr from the forwarding headers.
package realip

import (
	"net"
	"net/http"
	"strings"
)

// New returns a middleware that sets r.RemoteAddr to the client address from
// the RFC 7239 Forwarded header, falling back to the de-facto X-Forwarded-For,
// X-Real-IP, True-Client-IP and CF-Connecting-IP headers. The original port is
// kept when there is one.
//
// It trusts those headers. Only put it behind a proxy that sets them, or a
// client can spoof its own address.
func New() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ip := clientIP(r); ip != "" {
				r.RemoteAddr = ip + port(r.RemoteAddr)
			}
			next.ServeHTTP(w, r)
		})
	}
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("Forwarded"); fwd != "" {
		if ip := forwardedFor(fwd); ip != "" {
			return ip
		}
	}
	// Legacy, de-facto headers (no RFC). X-Forwarded-For is client-first.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		if ip := net.ParseIP(strings.TrimSpace(xff)); ip != nil {
			return ip.String()
		}
	}
	for _, h := range []string{"X-Real-IP", "True-Client-IP", "CF-Connecting-IP"} {
		if ip := net.ParseIP(strings.TrimSpace(r.Header.Get(h))); ip != nil {
			return ip.String()
		}
	}
	return ""
}

// forwardedFor returns the client address from an RFC 7239 Forwarded header.
// The list is ordered client-first, so the first "for=" is the client.
func forwardedFor(h string) string {
	for _, elem := range splitOutside(h, ',') {
		for _, pair := range splitOutside(elem, ';') {
			k, v, ok := strings.Cut(pair, "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(k), "for") {
				continue
			}
			return parseNode(strings.TrimSpace(v))
		}
	}
	return ""
}

// parseNode parses an RFC 7239 node identifier: an IP, "ip:port", "[v6]:port",
// or an obfuscated identifier ("_hidden", which carries no address).
func parseNode(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
		v = strings.ReplaceAll(v, `\"`, `"`)
		v = strings.ReplaceAll(v, `\\`, `\`)
	}
	if v == "" || v[0] == '_' {
		return ""
	}
	host := v
	if h, _, err := net.SplitHostPort(v); err == nil {
		host = h
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return ip.String()
	}
	return ""
}

// splitOutside splits s on sep, ignoring separators inside quoted strings, per
// RFC 9110's list/quoted-string grammar.
func splitOutside(s string, sep byte) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && inQuote && i+1 < len(s):
			cur.WriteByte(c)
			i++
			cur.WriteByte(s[i])
		case c == '"':
			inQuote = !inQuote
			cur.WriteByte(c)
		case c == sep && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(out, cur.String())
}

func port(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		return addr[i:]
	}
	return ""
}
