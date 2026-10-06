package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	sqlitestore "github.com/prem0x01/costblame/internal/store/sqlite"
	"github.com/prem0x01/costblame/pkg/models"
)

var uiBase = time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// seedAnomaly stores an anomaly for the service, `day` days after a fixed date.
func seedAnomaly(t *testing.T, s *sqlitestore.Store, service string, day int) models.CostSnapshot {
	t.Helper()
	start := uiBase.AddDate(0, 0, day)
	snap := models.CostSnapshot{
		ID: uuid.New(), CollectedAt: start, PeriodStart: start, PeriodEnd: start.Add(24 * time.Hour),
		Source: "aws", Service: service, AmountUSD: 500, PrevAmountUSD: 100, DeltaPct: 400, IsAnomaly: true,
		AnomalyScore: 5, Granularity: models.GranularityDaily,
	}
	if err := s.SaveCostSnapshots(context.Background(), []models.CostSnapshot{snap}); err != nil {
		t.Fatal(err)
	}
	return snap
}

// seedEdge links the anomaly to a new deploy by `author` with the given status and score.
func seedEdge(t *testing.T, s *sqlitestore.Store, snap models.CostSnapshot, author string, status models.BlameStatus, score float64) models.BlameEdge {
	t.Helper()
	ctx := context.Background()
	d := models.DeployEvent{
		ID: uuid.New(), OccurredAt: snap.PeriodStart.Add(-time.Hour), Source: models.DeploySourceGitHubActions,
		Repository: "acme/payments", CommitSHA: uuid.NewString()[:8], PRAuthor: author, Status: models.DeployStatusSuccess,
	}
	if err := s.UpsertDeployEvent(ctx, d); err != nil {
		t.Fatal(err)
	}
	e := models.BlameEdge{
		ID: uuid.New(), CostSnapshotID: snap.ID, DeployEventID: d.ID, ConfidenceScore: score,
		Status: models.BlameStatusPending, CreatedAt: time.Now().UTC(),
	}
	if err := s.SaveBlameEdges(ctx, []models.BlameEdge{e}); err != nil {
		t.Fatal(err)
	}
	if status != models.BlameStatusPending {
		if err := s.UpdateBlameStatus(ctx, e.ID, status); err != nil {
			t.Fatal(err)
		}
	}
	e.Status = status
	return e
}

// The bug from the issue: an anomaly whose only candidates scored below the
// alert threshold used to disappear from the Anomalies page and the blame list.
func TestAnomalies_PendingOnlyAnomalyIsListed(t *testing.T) {
	srv, store := newTestServer(t)
	snap := seedAnomaly(t, store, "Amazon Simple Storage Service", 0)
	seedEdge(t, store, snap, "alice", models.BlameStatusPending, 0.42)

	code, body := get(t, srv.URL+"/anomalies")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, "Amazon Simple Storage Service") {
		t.Fatalf("an anomaly with only pending candidates vanished from the list:\n%s", body)
	}
	if !strings.Contains(body, "badge-state-candidates") || !strings.Contains(body, "1 pending") {
		t.Errorf("the row should show its state and pending count")
	}
	if !strings.Contains(body, "/anomalies/"+snap.ID.String()) {
		t.Error("the row should link to the anomaly's detail page")
	}
	if !strings.Contains(body, "42%") {
		t.Error("the best candidate's score should be shown")
	}
}

func TestAnomalies_StateFilterAndTabCounts(t *testing.T) {
	srv, store := newTestServer(t)
	unblamed := seedAnomaly(t, store, "AWS Lambda", 0)
	cand := seedAnomaly(t, store, "Amazon RDS", 1)
	seedEdge(t, store, cand, "alice", models.BlameStatusPending, 0.40)
	res := seedAnomaly(t, store, "Amazon DynamoDB", 2)
	seedEdge(t, store, res, "bob", models.BlameStatusResolved, 0.80)

	listed := func(url string) []string {
		_, body := get(t, url)
		var out []string
		for _, svc := range []string{"AWS Lambda", "Amazon RDS", "Amazon DynamoDB"} {
			if strings.Contains(body, ">"+svc+"</a>") {
				out = append(out, svc)
			}
		}
		return out
	}
	for url, want := range map[string][]string{
		"/anomalies":                  {"AWS Lambda", "Amazon RDS", "Amazon DynamoDB"},
		"/anomalies?state=all":        {"AWS Lambda", "Amazon RDS", "Amazon DynamoDB"},
		"/anomalies?state=candidates": {"Amazon RDS"},
		"/anomalies?state=unblamed":   {"AWS Lambda"},
		"/anomalies?state=resolved":   {"Amazon DynamoDB"},
		"/anomalies?state=confirmed":  nil,
	} {
		got := listed(srv.URL + url)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s lists %v, want %v", url, got, want)
		}
	}
	_ = unblamed

	_, body := get(t, srv.URL+"/anomalies")
	// Tabs carry exact counts: All 3, Candidates 1, Resolved 1, Unblamed 1, Confirmed 0, Dismissed 0.
	for label, count := range map[string]string{"All": "3", "Candidates": "1", "Resolved": "1", "Unblamed": "1", "Confirmed": "0", "Dismissed": "0"} {
		if !strings.Contains(body, label+` <span class="tab-count">`+count+`</span>`) {
			t.Errorf("tab %q should show count %s", label, count)
		}
	}
	if !strings.Contains(body, `tab tab-active" href="/anomalies?state=all"`) {
		t.Error("the All tab should be marked active by default")
	}

	if code, _ := get(t, srv.URL+"/anomalies?state=bogus"); code != http.StatusBadRequest {
		t.Errorf("unknown state: status = %d, want 400", code)
	}
	code, body := get(t, srv.URL+"/anomalies?state=confirmed")
	if code != http.StatusOK || !strings.Contains(body, "No anomalies are confirmed") {
		t.Errorf("an empty filter should explain itself (status %d)", code)
	}
}

func TestAnomalyDetail_ListsEveryCandidateRankedWithActions(t *testing.T) {
	srv, store := newTestServer(t)
	snap := seedAnomaly(t, store, "Amazon Simple Storage Service", 0)
	low := seedEdge(t, store, snap, "carol", models.BlameStatusPending, 0.30)
	high := seedEdge(t, store, snap, "alice", models.BlameStatusPending, 0.55)
	gone := seedEdge(t, store, snap, "dave", models.BlameStatusDismissed, 0.20)

	code, body := get(t, srv.URL+"/anomalies/"+snap.ID.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d\n%s", code, body)
	}
	for _, want := range []string{"Amazon Simple Storage Service", "Candidate deploys", "@alice", "@carol", "@dave", "2 still need a decision"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page is missing %q", want)
		}
	}
	// Best first.
	if strings.Index(body, "@alice") > strings.Index(body, "@carol") || strings.Index(body, "@carol") > strings.Index(body, "@dave") {
		t.Error("candidates are not ranked by confidence")
	}
	// Pending candidates can be confirmed or dismissed right here; dismissed ones cannot.
	if !strings.Contains(body, "/blame/"+high.ID.String()+"/confirm") || !strings.Contains(body, "/blame/"+low.ID.String()+"/dismiss") {
		t.Error("pending candidates need Confirm/Dismiss actions")
	}
	if strings.Contains(body, "/blame/"+gone.ID.String()+"/confirm") {
		t.Error("a dismissed candidate must not offer Confirm")
	}
	if !strings.Contains(body, "badge-state-candidates") {
		t.Error("the page should show the anomaly's state")
	}
}

func TestAnomalyDetail_NoCandidatesExplainsWhy(t *testing.T) {
	srv, store := newTestServer(t)
	snap := seedAnomaly(t, store, "AWS Lambda", 0)

	code, body := get(t, srv.URL+"/anomalies/"+snap.ID.String())
	if code != http.StatusOK || !strings.Contains(body, "No candidate deploys") || !strings.Contains(body, "badge-state-unblamed") {
		t.Errorf("status=%d; an anomaly with no candidates should say so\n%s", code, body)
	}
}

func TestAnomalyDetail_BadAndUnknownIDs(t *testing.T) {
	srv, _ := newTestServer(t)
	if code, _ := get(t, srv.URL+"/anomalies/not-a-uuid"); code != http.StatusBadRequest {
		t.Errorf("malformed id: status = %d, want 400", code)
	}
	if code, _ := get(t, srv.URL+"/anomalies/"+uuid.NewString()); code != http.StatusNotFound {
		t.Errorf("unknown id: status = %d, want 404", code)
	}
}

func TestBlameList_DefaultIncludesPendingAndFiltersByStatus(t *testing.T) {
	srv, store := newTestServer(t)
	snap := seedAnomaly(t, store, "Amazon RDS", 0)
	seedEdge(t, store, snap, "pendingpat", models.BlameStatusPending, 0.40)
	seedEdge(t, store, snap, "resolvedrob", models.BlameStatusResolved, 0.80)
	seedEdge(t, store, snap, "dismisseddan", models.BlameStatusDismissed, 0.20)

	_, body := get(t, srv.URL+"/blame")
	if !strings.Contains(body, "@pendingpat") || !strings.Contains(body, "@resolvedrob") {
		t.Error("the default list must include pending as well as resolved edges")
	}
	if strings.Contains(body, "@dismisseddan") {
		t.Error("dismissed edges are hidden by default")
	}

	for url, want := range map[string]string{
		"/blame?status=pending":   "@pendingpat",
		"/blame?status=resolved":  "@resolvedrob",
		"/blame?status=dismissed": "@dismisseddan",
	} {
		_, body := get(t, srv.URL+url)
		for _, who := range []string{"@pendingpat", "@resolvedrob", "@dismisseddan"} {
			if has := strings.Contains(body, who); has != (who == want) {
				t.Errorf("%s: contains %s = %v, want %v", url, who, has, who == want)
			}
		}
	}
	_, body = get(t, srv.URL+"/blame?status=all")
	if !strings.Contains(body, "@dismisseddan") {
		t.Error("status=all includes dismissed edges")
	}
	if code, _ := get(t, srv.URL+"/blame?status=bogus"); code != http.StatusBadRequest {
		t.Errorf("unknown status: %d, want 400", code)
	}
	// Tab counts are exact.
	_, body = get(t, srv.URL+"/blame")
	for label, count := range map[string]string{"Active": "2", "Pending": "1", "Resolved": "1", "Confirmed": "0", "Dismissed": "1", "All": "3"} {
		if !strings.Contains(body, label+` <span class="tab-count">`+count+`</span>`) {
			t.Errorf("blame tab %q should show %s", label, count)
		}
	}
}

// The pending row itself needs the actions, and acting on it must work.
func TestBlameList_PendingRowCanBeConfirmedInPlace(t *testing.T) {
	srv, store := newTestServer(t)
	snap := seedAnomaly(t, store, "Amazon RDS", 0)
	e := seedEdge(t, store, snap, "alice", models.BlameStatusPending, 0.40)

	_, body := get(t, srv.URL+"/blame")
	if !strings.Contains(body, "/blame/"+e.ID.String()+"/confirm") {
		t.Fatal("a pending row must offer Confirm")
	}

	resp, err := http.PostForm(srv.URL+"/blame/"+e.ID.String()+"/confirm", map[string][]string{"view": {"row"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "badge-confirmed") || strings.Contains(string(b), "btn-confirm") {
		t.Errorf("confirming a pending row: status=%d body=%s", resp.StatusCode, b)
	}
	if got, _ := store.BlameEdgeByID(context.Background(), e.ID); got.Status != models.BlameStatusConfirmed {
		t.Errorf("stored status = %q, want confirmed", got.Status)
	}
}

func TestBlameDetail_PendingEdgeOffersActionsAndLinksToTheAnomaly(t *testing.T) {
	srv, store := newTestServer(t)
	snap := seedAnomaly(t, store, "Amazon RDS", 0)
	e := seedEdge(t, store, snap, "alice", models.BlameStatusPending, 0.40)

	code, body := get(t, srv.URL+"/blame/"+e.ID.String())
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, "/blame/"+e.ID.String()+"/confirm") {
		t.Error("a pending edge's page must offer Confirm")
	}
	if !strings.Contains(body, "/anomalies/"+snap.ID.String()) || !strings.Contains(body, "Candidate deployment") {
		t.Error("the page should link to the anomaly and call a pending edge a candidate")
	}
}

// Dashboard figures used to come from the latest 100 edges and were wrong beyond
// that. They are exact counts now, and link to the matching filtered list.
func TestDashboard_CountsAreExactBeyondAHundred(t *testing.T) {
	srv, store := newTestServer(t)
	for i := 0; i < 120; i++ {
		snap := seedAnomaly(t, store, fmt.Sprintf("Service-%d", i), i)
		seedEdge(t, store, snap, "alice", models.BlameStatusPending, 0.4)
	}

	code, body := get(t, srv.URL+"/")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, `data-countup="120"`) {
		t.Errorf("the candidates card should read exactly 120:\n%s", body)
	}
	for _, link := range []string{`href="/anomalies?state=candidates"`, `href="/anomalies?state=unblamed"`, `href="/blame?status=resolved"`, `href="/blame?status=confirmed"`} {
		if !strings.Contains(body, link) {
			t.Errorf("dashboard is missing the link %s", link)
		}
	}
}
