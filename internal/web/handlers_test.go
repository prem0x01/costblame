package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	sqlitestore "github.com/prem0x01/costblame/internal/store/sqlite"
	"github.com/prem0x01/costblame/internal/web"
	"github.com/prem0x01/costblame/pkg/models"
)

// readyStatus is a fully-configured SystemStatus, used by tests that exercise
// pages assuming setup is complete.
var readyStatus = web.SystemStatus{
	CostProvider:    "aws",
	DeploySources:   []string{"github"},
	NarrativeEngine: "template",
}

func newTestServerWithStatus(t *testing.T, status web.SystemStatus) (*httptest.Server, *sqlitestore.Store) {
	t.Helper()
	f, err := os.CreateTemp("", "costblame-web-test-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	store, err := sqlitestore.New(f.Name())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	mux := http.NewServeMux()
	web.New(store, status).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store
}

func newTestServer(t *testing.T) (*httptest.Server, *sqlitestore.Store) {
	t.Helper()
	return newTestServerWithStatus(t, readyStatus)
}

func TestDashboard_Empty(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestBlameDetail_ConfirmSwapsBadge(t *testing.T) {
	srv, store := newTestServer(t)
	ctx := context.Background()

	snapID, deployID, edgeID := uuid.New(), uuid.New(), uuid.New()

	if err := store.SaveCostSnapshots(ctx, []models.CostSnapshot{{
		ID:          snapID,
		CollectedAt: time.Now().UTC(),
		PeriodStart: time.Now().UTC().Add(-1 * time.Hour),
		PeriodEnd:   time.Now().UTC(),
		Source:      "aws",
		Service:     "AmazonECS",
		AmountUSD:   500,
		IsAnomaly:   true,
		Granularity: models.GranularityHourly,
	}}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	if err := store.UpsertDeployEvent(ctx, models.DeployEvent{
		ID:         deployID,
		OccurredAt: time.Now().UTC().Add(-30 * time.Minute),
		Source:     models.DeploySourceGitHubActions,
		Repository: "acme/checkout",
		PRAuthor:   "alice",
		Status:     models.DeployStatusSuccess,
	}); err != nil {
		t.Fatalf("seed deploy event: %v", err)
	}
	if err := store.SaveBlameEdges(ctx, []models.BlameEdge{{
		ID:              edgeID,
		CostSnapshotID:  snapID,
		DeployEventID:   deployID,
		ConfidenceScore: 0.9,
		Narrative:       "Cost for AmazonECS spiked.",
		Status:          models.BlameStatusResolved,
		CreatedAt:       time.Now().UTC(),
	}}); err != nil {
		t.Fatalf("seed blame edge: %v", err)
	}

	// Detail page renders the hydrated fields.
	resp, err := http.Get(srv.URL + "/blame/" + edgeID.String())
	if err != nil {
		t.Fatalf("GET /blame/{id}: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "AmazonECS") || !strings.Contains(body, "alice") {
		t.Errorf("detail page missing hydrated fields, got: %s", body)
	}

	// Confirming via the "badge" view returns the status_badge partial with
	// no action buttons left (status is no longer "resolved").
	confirmResp, err := http.PostForm(srv.URL+"/blame/"+edgeID.String()+"/confirm", map[string][]string{
		"view": {"badge"},
	})
	if err != nil {
		t.Fatalf("POST confirm: %v", err)
	}
	confirmBodyBytes, _ := io.ReadAll(confirmResp.Body)
	confirmBody := string(confirmBodyBytes)
	confirmResp.Body.Close()
	if confirmResp.StatusCode != http.StatusOK {
		t.Fatalf("confirm status = %d, want 200; body=%s", confirmResp.StatusCode, confirmBody)
	}
	if !strings.Contains(confirmBody, "badge-confirmed") {
		t.Errorf("expected confirmed badge in response, got: %s", confirmBody)
	}
	if strings.Contains(confirmBody, "btn-confirm") {
		t.Errorf("expected confirm button removed after confirming, got: %s", confirmBody)
	}
}

func TestDashboard_RedirectsToSetupWhenNotReady(t *testing.T) {
	srv, _ := newTestServerWithStatus(t, web.SystemStatus{}) // nothing configured

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/setup" {
		t.Errorf("Location = %q, want /setup", loc)
	}
}

func TestSetup_ShowsMissingPiecesWhenNotReady(t *testing.T) {
	srv, _ := newTestServerWithStatus(t, web.SystemStatus{})

	resp, err := http.Get(srv.URL + "/setup")
	if err != nil {
		t.Fatalf("GET /setup: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, text)
	}
	if strings.Contains(text, "You're all set") {
		t.Errorf("expected setup-incomplete state, got the all-set hero: %s", text)
	}
	if !strings.Contains(text, "Not connected") {
		t.Errorf("expected at least one \"Not connected\" status, got: %s", text)
	}
}

func TestSetup_ShowsReadyWhenConfigured(t *testing.T) {
	srv, _ := newTestServerWithStatus(t, readyStatus)

	resp, err := http.Get(srv.URL + "/setup")
	if err != nil {
		t.Fatalf("GET /setup: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)

	if !strings.Contains(text, "You're all set") {
		t.Errorf("expected the all-set hero, got: %s", text)
	}
}
