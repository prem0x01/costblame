package argocd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const syncedApp = `{"metadata":{"name":"payments"},"status":{"operationState":{"phase":"Succeeded"}}}`

func post(h *WebhookHandler, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/argocd", strings.NewReader(syncedApp))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServeHTTP_RequiresBearerToken(t *testing.T) {
	h := New("argo-secret")

	for name, auth := range map[string]string{
		"missing header": "",
		"wrong token":    "Bearer wrong",
		"empty bearer":   "Bearer ",
	} {
		if rec := post(h, auth); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
	}
	if len(h.ch) != 0 {
		t.Fatalf("unauthenticated requests produced %d deploy event(s)", len(h.ch))
	}
}

func TestServeHTTP_AcceptsValidTokenAndEmitsDeploy(t *testing.T) {
	h := New("argo-secret")

	if rec := post(h, "Bearer argo-secret"); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	// parse runs in a goroutine after the 202; wait for the event.
	select {
	case ev := <-h.ch:
		if ev.Status == "" {
			t.Error("emitted event has no status")
		}
	case <-timeout():
		t.Fatal("no deploy event emitted for an authenticated request")
	}
}

func TestServeHTTP_EmptyTokenRejectsEverything(t *testing.T) {
	h := New("")
	// An empty configured token must not match a request that sends no token.
	if rec := post(h, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no header = %d, want 401", rec.Code)
	}
	if rec := post(h, "Bearer "); rec.Code != http.StatusUnauthorized {
		t.Errorf("empty bearer = %d, want 401", rec.Code)
	}
}

func timeout() <-chan time.Time { return time.After(2 * time.Second) }

func syncOf(app, revision string) string {
	return `{"metadata":{"name":"` + app + `"},"spec":{"source":{"repoURL":"https://git.example/acme/infra"}},` +
		`"status":{"sync":{"revision":"` + revision + `"},"operationState":{"phase":"Succeeded","finishedAt":"2026-07-08T10:00:00Z"}}}`
}

func postBody(h *WebhookHandler, body string) {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/argocd", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer argo-secret")
	h.ServeHTTP(httptest.NewRecorder(), req)
}

func TestSameAppAndRevisionIsOneDeploy(t *testing.T) {
	h := New("argo-secret")
	postBody(h, syncOf("payments", "rev1"))
	postBody(h, syncOf("payments", "rev1")) // resync / redelivery

	var ids [2]string
	for i := range ids {
		select {
		case ev := <-h.ch:
			ids[i] = ev.ID.String()
		case <-timeout():
			t.Fatalf("event %d never arrived", i)
		}
	}
	if ids[0] != ids[1] {
		t.Errorf("resync of one revision got different IDs: %v", ids)
	}
}

func TestDifferentAppsOrRevisionsAreDifferentDeploys(t *testing.T) {
	h := New("argo-secret")
	postBody(h, syncOf("payments", "rev1"))
	postBody(h, syncOf("billing", "rev1")) // several apps can share a repo and revision
	postBody(h, syncOf("payments", "rev2"))

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		select {
		case ev := <-h.ch:
			seen[ev.ID.String()] = true
		case <-timeout():
			t.Fatalf("event %d never arrived", i)
		}
	}
	if len(seen) != 3 {
		t.Errorf("distinct (app, revision) pairs produced %d distinct IDs, want 3", len(seen))
	}
}
