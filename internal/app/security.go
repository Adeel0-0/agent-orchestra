package app

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
)

// localSecurity enforces the exact loopback authority and the launch token for HTTP
// and WebSocket requests (orchestra/app/security.py).
//
//   - Host must be exactly 127.0.0.1:<port>; an Origin, if sent, must be exactly
//     http://127.0.0.1:<port> (one header); Forwarded / X-Forwarded-* are refused.
//   - The token comes from exactly one "Authorization: Bearer <token>" header, or, for
//     a WebSocket upgrade only, from one Sec-WebSocket-Protocol header offering both
//     "orchestra" and exactly one "orchestra.token.<token>".
//   - public(r) requests (the UI shell, assets and nonce exchange) skip the token but
//     never the authority checks.
type localSecurity struct {
	next   http.Handler
	token  string
	host   string
	origin string
	csp    string
	public func(*http.Request) bool
}

func newLocalSecurity(next http.Handler, port int, token string, public func(*http.Request) bool) *localSecurity {
	host := fmt.Sprintf("127.0.0.1:%d", port)
	return &localSecurity{
		next: next, token: token, host: host, origin: "http://" + host, public: public,
		csp: "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self' ws://" +
			host + "; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'",
	}
}

// isWebSocket reports whether r asks for a WebSocket upgrade (an ASGI "websocket" scope).
func isWebSocket(r *http.Request) bool {
	return headerHasToken(r.Header, "Connection", "upgrade") && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func (s *localSecurity) authorityOK(r *http.Request) bool {
	// net/http moves Host into r.Host and rejects requests with several Host headers.
	if r.Host != s.host {
		return false
	}
	if origins, ok := r.Header["Origin"]; ok && (len(origins) != 1 || origins[0] != s.origin) {
		return false
	}
	for name := range r.Header {
		lower := strings.ToLower(name)
		if lower == "forwarded" || strings.HasPrefix(lower, "x-forwarded-") {
			return false
		}
	}
	return true
}

func (s *localSecurity) suppliedToken(r *http.Request, websocket bool) string {
	auth := r.Header.Values("Authorization")
	if len(auth) == 1 && strings.HasPrefix(auth[0], "Bearer ") && len(auth[0]) > len("Bearer ") {
		return auth[0][len("Bearer "):]
	}
	if !websocket {
		return ""
	}
	protocols := r.Header.Values("Sec-WebSocket-Protocol")
	if len(protocols) != 1 {
		return ""
	}
	var tokens []string
	offered := false
	for _, p := range strings.Split(protocols[0], ",") {
		p = strings.TrimSpace(p)
		if p == "orchestra" {
			offered = true
		}
		if strings.HasPrefix(p, "orchestra.token.") {
			tokens = append(tokens, p[len("orchestra.token."):])
		}
	}
	if offered && len(tokens) == 1 {
		return tokens[0]
	}
	return ""
}

func (s *localSecurity) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	websocket := isWebSocket(r)
	authority := s.authorityOK(r)
	supplied := s.suppliedToken(r, websocket)
	authenticated := supplied != "" && isASCII(supplied) &&
		subtle.ConstantTimeCompare([]byte(supplied), []byte(s.token)) == 1
	public := !websocket && s.public != nil && s.public(r)
	if !authority || !(authenticated || public) {
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", "no-store")
		status := http.StatusUnauthorized
		if !authority || websocket {
			// A WebSocket closed before accept is an HTTP 403, as with uvicorn.
			status = http.StatusForbidden
		}
		w.WriteHeader(status)
		w.Write([]byte(`{"error":"Local authorization required"}`))
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", s.csp)
	s.next.ServeHTTP(w, r)
}
