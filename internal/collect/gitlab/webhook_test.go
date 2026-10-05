package gitlab

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prem0x01/costblame/pkg/models"
)

func pipeline(ref, sha, status string) string {
	return `{"object_kind":"pipeline","object_attributes":{"status":"` + status + `","ref":"` + ref + `","sha":"` + sha + `"},` +
		`"project":{"path_with_namespace":"acme/payments"},` +
		`"commit":{"timestamp":"2026-07-08T10:00:00Z"}}`
}

func post(h *WebhookHandler, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Gitlab-Token", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// parse runs in a goroutine after the 202; wait for an event, or report none.
func next(h *WebhookHandler, wait time.Duration) (models.DeployEvent, bool) {
	select {
	case ev := <-h.ch:
		return ev, true
	case <-time.After(wait):
		return models.DeployEvent{}, false
	}
}

func TestPipelineRetriesShareOneDeployID(t *testing.T) {
	h := New("tok")
	body := pipeline("main", "abc123", "success")

	for i := 0; i < 2; i++ {
		if rec := post(h, "tok", body); rec.Code != http.StatusAccepted {
			t.Fatalf("delivery %d: status = %d, want 202", i, rec.Code)
		}
	}
	a, ok1 := next(h, 2*time.Second)
	b, ok2 := next(h, 2*time.Second)
	if !ok1 || !ok2 {
		t.Fatal("expected two events (one per delivery)")
	}
	if a.ID != b.ID {
		t.Errorf("redelivery produced different IDs %s / %s", a.ID, b.ID)
	}
	if want := models.DeployEventID(models.DeploySourceGitLabCI, "acme/payments", "abc123"); a.ID != want {
		t.Errorf("ID = %s, want %s", a.ID, want)
	}
}

func TestIgnoredPipelines(t *testing.T) {
	for name, body := range map[string]string{
		"failed":         pipeline("main", "abc123", "failed"),
		"feature branch": pipeline("feature/x", "abc123", "success"),
		"no commit sha":  pipeline("main", "", "success"),
	} {
		t.Run(name, func(t *testing.T) {
			h := New("tok")
			if rec := post(h, "tok", body); rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", rec.Code)
			}
			if ev, ok := next(h, 150*time.Millisecond); ok {
				t.Fatalf("emitted an event for an ignored pipeline: %+v", ev)
			}
		})
	}
}

func TestEmptySecretRejectsEverything(t *testing.T) {
	h := New("")
	if rec := post(h, "", pipeline("main", "abc123", "success")); rec.Code != http.StatusUnauthorized {
		t.Errorf("no header = %d, want 401 (an empty secret must fail closed)", rec.Code)
	}
}

func TestWrongTokenRejected(t *testing.T) {
	h := New("tok")
	if rec := post(h, "nope", pipeline("main", "abc123", "success")); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
