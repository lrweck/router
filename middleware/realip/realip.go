// Package realip rewrites the request's RemoteAddr from the forwarding headers.
package realip

import (
	"net/http"
	"net/netip"
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

// clientIP returns a valid address text, or "" to leave RemoteAddr alone. The
// Forwarded parser only slices the header, so this allocates nothing.
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
		if ip, ok := parseAddr(strings.TrimSpace(xff)); ok {
			return ip
		}
	}
	for _, h := range [...]string{"X-Real-IP", "True-Client-IP", "CF-Connecting-IP"} {
		if ip, ok := parseAddr(strings.TrimSpace(r.Header.Get(h))); ok {
			return ip
		}
	}
	return ""
}

// forwardedFor returns the client address from an RFC 7239 Forwarded header.
// The list is ordered client-first, so the first "for=" is the client.
func forwardedFor(h string) string {
	for elem := h; ; {
		e, rest := cutOutside(elem, ',')
		if ip := forOf(e); ip != "" {
			return ip
		}
		if rest == "" {
			return ""
		}
		elem = rest
	}
}

func forOf(elem string) string {
	for pair := elem; ; {
		p, rest := cutOutside(pair, ';')
		if k, v, ok := strings.Cut(p, "="); ok && strings.EqualFold(strings.TrimSpace(k), "for") {
			return parseNode(strings.TrimSpace(v))
		}
		if rest == "" {
			return ""
		}
		pair = rest
	}
}

// cutOutside cuts s at the first sep that is not inside a quoted string,
// following RFC 9110's list/quoted-string grammar. It allocates nothing.
func cutOutside(s string, sep byte) (before, after string) {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\\' && inQuote:
			i++ // the escaped byte cannot close the string
		case c == '"':
			inQuote = !inQuote
		case c == sep && !inQuote:
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

// parseNode parses an RFC 7239 node identifier: an IP, "ip:port", "[v6]:port",
// or an obfuscated identifier ("_hidden", which carries no address).
func parseNode(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = unescape(v[1 : len(v)-1])
	}
	if v == "" || v[0] == '_' {
		return ""
	}
	host := v
	switch v[0] {
	case '[': // "[v6]" or "[v6]:port"
		if i := strings.IndexByte(v, ']'); i > 0 {
			host = v[1:i]
		}
	default:
		// "ip:port" has exactly one colon; a bare IPv6 has several, so it
		// falls through with host == v.
		if i := strings.IndexByte(v, ':'); i >= 0 && strings.LastIndexByte(v, ':') == i {
			host = v[:i]
		}
	}
	if ip, ok := parseAddr(host); ok {
		return ip
	}
	return ""
}

// parseAddr reports whether s is a valid address, returning it unchanged so no
// string is allocated (netip parses into a value).
func parseAddr(s string) (string, bool) {
	if _, err := netip.ParseAddr(s); err != nil {
		return "", false
	}
	return s, true
}

// unescape resolves the quoted-pair escapes of a quoted-string. It returns the
// input unchanged (no allocation) when there is nothing to unescape.
func unescape(v string) string {
	if !strings.ContainsRune(v, '\\') {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); i++ {
		if v[i] == '\\' && i+1 < len(v) {
			i++
		}
		b.WriteByte(v[i])
	}
	return b.String()
}

func port(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		return addr[i:]
	}
	return ""
}
