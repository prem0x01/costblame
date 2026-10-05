package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
)

// isPublicRoute reports whether the mux route a request resolves to is served
// without the API token. It takes the registered pattern, not the URL path:
// the UI mounts a catch-all "GET /", so judging by path prefix would let
// "GET /webhooks/github" (no such GET route) fall through to the dashboard
// without authentication.
//
// Public: webhook receivers (POST only; they verify their own HMAC/token),
// the health probe, and the UI's embedded static assets. An empty pattern
// (no route, or method not allowed) is private.
func isPublicRoute(pattern string) bool {
	return pattern == "GET /healthz" ||
		strings.HasPrefix(pattern, "GET /static/") ||
		strings.HasPrefix(pattern, "POST /webhooks/")
}

// protect wraps next with the two checks every non-public request needs:
//
//   - authentication: a bearer token, or HTTP Basic auth with the token as the
//     password (any username) so a browser gets a native login prompt and curl
//     can use either form;
//   - cross-site request protection for state-changing methods. Browsers
//     replay cached Basic credentials on cross-site requests, so a token alone
//     doesn't stop CSRF.
//
// Webhook routes skip both: they are called by CI systems, not browsers, and
// verify their own signatures.
//
// Whether a request is public is decided by asking mux which route it would
// dispatch to, so the check can never disagree with the router about what a
// path means (trailing slashes, encoded slashes, dot segments, method).
func protect(token string, mux *http.ServeMux) http.Handler {
	want := sha256.Sum256([]byte(token))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); isPublicRoute(pattern) {
			mux.ServeHTTP(w, r)
			return
		}

		// Fail closed: an empty token would otherwise match an empty credential.
		if token == "" {
			http.Error(w, "server has no API token configured", http.StatusServiceUnavailable)
			return
		}

		if !authorized(r, want) {
			w.Header().Set("WWW-Authenticate", `Basic realm="costblame", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if isStateChanging(r.Method) && !sameOrigin(r) {
			http.Error(w, "cross-origin request blocked", http.StatusForbidden)
			return
		}

		mux.ServeHTTP(w, r)
	})
}

// authorized reports whether r carries the API token. Candidates are compared
// by SHA-256 digest in constant time, so neither the token's content nor its
// length leaks through timing.
func authorized(r *http.Request, want [sha256.Size]byte) bool {
	var candidate string
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		candidate = strings.TrimPrefix(h, "Bearer ")
	} else if _, pass, ok := r.BasicAuth(); ok {
		candidate = pass
	} else {
		return false
	}
	got := sha256.Sum256([]byte(candidate))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func isStateChanging(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// sameOrigin rejects requests a browser marks as cross-site. Non-browser
// clients (curl, scripts) send neither header and are allowed — they had to
// present the token to get here. This mirrors the approach of Go's
// http.CrossOriginProtection, reimplemented because go.mod targets Go 1.22.
func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	return true
}

// securityHeaders sets conservative browser hardening headers on every
// response. The CSP allows inline styles because htmx injects a small
// indicator stylesheet; scripts are restricted to same-origin files.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
				"frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}
