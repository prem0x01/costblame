package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testToken = "s3cret-token"
	secret    = "SECRET-DASHBOARD-CONTENT"
)

// testMux mirrors the real route shapes, including the UI's catch-all, which
// is what a path-prefix auth check gets wrong. The catch-all writes `secret` so
// tests can assert nothing private leaks.
func testMux() *http.ServeMux {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	mux := http.NewServeMux()
	mux.Handle("GET /api/blame", ok)
	mux.Handle("POST /api/blame/{id}/confirm", ok)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, secret) })
	mux.Handle("GET /static/", ok)
	mux.Handle("POST /webhooks/github", ok)
	mux.Handle("GET /healthz", ok)
	return mux
}

func do(h http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestProtect_RequiresToken(t *testing.T) {
	h := protect(testToken, testMux())

	t.Run("no credentials", func(t *testing.T) {
		rec := do(h, http.MethodGet, "/api/blame", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Error("missing WWW-Authenticate challenge, browsers won't prompt for a login")
		}
	})

	t.Run("wrong token", func(t *testing.T) {
		rec := do(h, http.MethodGet, "/api/blame", map[string]string{"Authorization": "Bearer nope"})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("bearer token", func(t *testing.T) {
		rec := do(h, http.MethodGet, "/api/blame", map[string]string{"Authorization": "Bearer " + testToken})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("basic auth, token as password", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.SetBasicAuth("anyone", testToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != secret {
			t.Fatalf("status = %d body = %q, want 200 + dashboard", rec.Code, rec.Body.String())
		}
	})

	t.Run("UI routes are protected too", func(t *testing.T) {
		for _, p := range []string{"/", "/blame", "/blame/abc", "/anomalies", "/setup", "/typo"} {
			rec := do(h, http.MethodGet, p, nil)
			if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), secret) {
				t.Errorf("GET %s = %d, want 401 and no content", p, rec.Code)
			}
		}
	})
}

func TestProtect_EmptyTokenFailsClosed(t *testing.T) {
	h := protect("", testMux())

	// An empty credential must not match an empty token.
	for _, auth := range []string{"", "Bearer ", "Bearer"} {
		hdr := map[string]string{}
		if auth != "" {
			hdr["Authorization"] = auth
		}
		if rec := do(h, http.MethodGet, "/api/blame", hdr); rec.Code == http.StatusOK {
			t.Errorf("Authorization %q got through with no token configured", auth)
		}
	}
}

func TestProtect_PublicRoutes(t *testing.T) {
	h := protect(testToken, testMux())

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/healthz"},
		{http.MethodPost, "/webhooks/github"},
		{http.MethodGet, "/static/style.css"},
	} {
		if rec := do(h, tc.method, tc.path, nil); rec.Code != http.StatusOK {
			t.Errorf("%s %s = %d, want 200 (public)", tc.method, tc.path, rec.Code)
		}
	}
}

// The UI's catch-all "GET /" matches paths that merely *look* public. Auth must
// follow the route the mux picks, not the URL prefix.
func TestProtect_LookalikePublicPathsDoNotReachCatchAll(t *testing.T) {
	h := protect(testToken, testMux())

	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/webhooks/github"},   // webhook route is POST-only
		{http.MethodHead, "/webhooks/github"},  // HEAD maps to GET routes
		{http.MethodGet, "/webhooks"},          // prefix without a route
		{http.MethodGet, "/webhooks/"},         //
		{http.MethodGet, "/healthz/"},          // exact route has no trailing slash
		{http.MethodGet, "/webhooks%2Fgithub"}, // encoded slash
		{http.MethodPost, "/healthz"},          // wrong method for the public route
		{http.MethodGet, "/webhooks/../api/blame"},
		{http.MethodGet, "/static/../api/blame"},
		{http.MethodGet, "/healthz/../api/blame"},
	} {
		rec := do(h, tc.method, tc.target, nil)
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), secret) {
			t.Errorf("%s %s = %d body=%q: reached private content without a token",
				tc.method, tc.target, rec.Code, rec.Body.String())
		}
	}
}

func TestProtect_BlocksCrossSiteStateChanges(t *testing.T) {
	h := protect(testToken, testMux())
	auth := "Bearer " + testToken

	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"curl-style POST, no browser headers", http.MethodPost,
			map[string]string{"Authorization": auth}, http.StatusOK},
		{"same-origin browser POST", http.MethodPost,
			map[string]string{"Authorization": auth, "Origin": "http://example.com", "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"cross-site Origin", http.MethodPost,
			map[string]string{"Authorization": auth, "Origin": "https://evil.test"}, http.StatusForbidden},
		{"cross-site Sec-Fetch-Site", http.MethodPost,
			map[string]string{"Authorization": auth, "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site (sibling subdomain) is not same-origin", http.MethodPost,
			map[string]string{"Authorization": auth, "Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"malformed Origin", http.MethodPost,
			map[string]string{"Authorization": auth, "Origin": "://bad"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// httptest requests default to Host "example.com".
			rec := do(h, tc.method, "/api/blame/00000000-0000-0000-0000-000000000000/confirm", tc.headers)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}

	t.Run("GET is never blocked by origin checks", func(t *testing.T) {
		rec := do(h, http.MethodGet, "/api/blame",
			map[string]string{"Authorization": auth, "Sec-Fetch-Site": "cross-site"})
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})
}

func TestProtect_WebhooksSkipOriginCheck(t *testing.T) {
	h := protect(testToken, testMux())
	rec := do(h, http.MethodPost, "/webhooks/github", map[string]string{"Origin": "https://github.com"})
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook POST with foreign Origin = %d, want 200 (receivers verify their own signature)", rec.Code)
	}
}

// stubUI registers the same catch-all shape web.Handler used to, so New is
// tested against the route table that exposed the bypass.
type stubUI struct{ root string }

func (u stubUI) Register(mux *http.ServeMux) {
	mux.HandleFunc(u.root, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, secret) })
	mux.HandleFunc("GET /static/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "asset") })
}

func TestNew_WiresProtectionAndHeaders(t *testing.T) {
	webhook := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusAccepted) })

	// Both the old catch-all root and the anchored one must be safe.
	for _, root := range []string{"GET /", "GET /{$}"} {
		t.Run(root, func(t *testing.T) {
			srv := New(":0", testToken, nil, map[string]http.Handler{"github": webhook}, stubUI{root: root})
			h := srv.httpServer.Handler

			rec := do(h, http.MethodGet, "/api/blame", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated /api/blame = %d, want 401", rec.Code)
			}
			if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("X-Frame-Options") != "DENY" ||
				rec.Header().Get("Content-Security-Policy") == "" {
				t.Errorf("security headers missing on error response: %v", rec.Header())
			}

			for _, tc := range []struct{ method, target string }{
				{http.MethodGet, "/"},
				{http.MethodGet, "/webhooks/github"},
				{http.MethodGet, "/webhooks"},
				{http.MethodGet, "/healthz/"},
				{http.MethodGet, "/webhooks%2Fgithub"},
				{http.MethodHead, "/webhooks/github"},
			} {
				rec := do(h, tc.method, tc.target, nil)
				if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), secret) {
					t.Errorf("%s %s = %d: private content reachable without a token", tc.method, tc.target, rec.Code)
				}
			}

			if rec := do(h, http.MethodGet, "/healthz", nil); rec.Code != http.StatusOK {
				t.Errorf("/healthz = %d, want 200", rec.Code)
			}
			if rec := do(h, http.MethodGet, "/static/app.js", nil); rec.Code != http.StatusOK {
				t.Errorf("/static/app.js = %d, want 200", rec.Code)
			}
			if rec := do(h, http.MethodPost, "/webhooks/github", nil); rec.Code != http.StatusAccepted {
				t.Errorf("POST /webhooks/github = %d, want 202 from the receiver", rec.Code)
			}
			rec = do(h, http.MethodGet, "/", map[string]string{"Authorization": "Bearer " + testToken})
			if rec.Code != http.StatusOK || rec.Body.String() != secret {
				t.Errorf("authenticated GET / = %d %q, want dashboard", rec.Code, rec.Body.String())
			}
		})
	}
}
