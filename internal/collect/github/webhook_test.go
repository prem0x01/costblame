package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prem0x01/costblame/internal/correlate"
	"github.com/prem0x01/costblame/pkg/models"
)

const testSecret = "s3cret"

var runTime = time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)

func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// send delivers a correctly signed webhook to h. The handler is built without a
// token in these tests, so no goroutine ever calls the real GitHub API.
func send(t *testing.T, h *WebhookHandler, event string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return sendRaw(h, event, body)
}

func sendRaw(h *WebhookHandler, event string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", sign([]byte(testSecret), body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func run(mod func(*WorkflowRun)) WorkflowRunPayload {
	r := WorkflowRun{
		ID: 1, Name: "Deploy", Path: ".github/workflows/deploy.yml",
		Event: "push", Status: "completed", Conclusion: "success",
		HeadBranch: "main", HeadSHA: "abc123", UpdatedAt: runTime,
	}
	if mod != nil {
		mod(&r)
	}
	return WorkflowRunPayload{Action: "completed", WorkflowRun: r, Repository: Repository{FullName: "Acme/Payments"}}
}

func drain(h *WebhookHandler) []models.DeployEvent {
	var out []models.DeployEvent
	for {
		select {
		case ev := <-h.events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"action":"completed"}`)

	h := NewWebhookHandler("s3cret", "", nil, nil)
	if !h.verifySignature(sign([]byte("s3cret"), body), body) {
		t.Error("valid signature rejected")
	}
	if h.verifySignature(sign([]byte("wrong"), body), body) {
		t.Error("signature with wrong key accepted")
	}
	if h.verifySignature("", body) {
		t.Error("missing signature header accepted")
	}

	// An empty secret must fail closed: anyone can compute an empty-key HMAC.
	empty := NewWebhookHandler("", "", nil, nil)
	if empty.verifySignature(sign(nil, body), body) {
		t.Error("empty-secret handler accepted a forgeable empty-key signature")
	}
}

func TestWorkflowRun_RedeliveryProducesSameEventID(t *testing.T) {
	h := NewWebhookHandler(testSecret, "", nil, nil)
	payload := run(nil)

	for i := 0; i < 2; i++ {
		if rec := send(t, h, "workflow_run", payload); rec.Code != http.StatusAccepted {
			t.Fatalf("delivery %d: status = %d, want 202", i, rec.Code)
		}
	}
	events := drain(h)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 (one per delivery)", len(events))
	}
	want := models.DeployEventID(models.DeploySourceGitHubActions, "Acme/Payments", "abc123")
	if events[0].ID != want || events[1].ID != want {
		t.Errorf("IDs = %s, %s; want both %s so the store upserts one row", events[0].ID, events[1].ID, want)
	}
	if events[0].Environment != "prod" || events[0].Repository != "Acme/Payments" || !events[0].OccurredAt.Equal(runTime) {
		t.Errorf("event fields = %+v", events[0])
	}
}

func TestWorkflowRun_SeveralWorkflowsForOneCommitShareAnID(t *testing.T) {
	h := NewWebhookHandler(testSecret, "", nil, nil) // no filter: every workflow counts

	send(t, h, "workflow_run", run(func(r *WorkflowRun) { r.ID, r.Name = 1, "CI" }))
	send(t, h, "workflow_run", run(func(r *WorkflowRun) { r.ID, r.Name = 2, "Lint" }))
	send(t, h, "workflow_run", run(func(r *WorkflowRun) { r.ID, r.Name = 3, "Deploy" }))

	events := drain(h)
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	for _, ev := range events[1:] {
		if ev.ID != events[0].ID {
			t.Errorf("workflows for one commit got different IDs: %s vs %s", ev.ID, events[0].ID)
		}
	}
}

func TestWorkflowRun_DifferentCommitsGetDifferentIDs(t *testing.T) {
	h := NewWebhookHandler(testSecret, "", nil, nil)
	send(t, h, "workflow_run", run(nil))
	send(t, h, "workflow_run", run(func(r *WorkflowRun) { r.HeadSHA = "def456" }))

	events := drain(h)
	if len(events) != 2 || events[0].ID == events[1].ID {
		t.Fatalf("distinct commits must be distinct deploys, got %d events", len(events))
	}
}

func TestWorkflowRun_IgnoresNonDeploys(t *testing.T) {
	cases := map[string]func(*WorkflowRun){
		// A nightly scan runs on main's HEAD, which may be days old.
		"scheduled run":            func(r *WorkflowRun) { r.Event = "schedule" },
		"pull_request run":         func(r *WorkflowRun) { r.Event = "pull_request" },
		"pull_request_target run":  func(r *WorkflowRun) { r.Event = "pull_request_target" },
		"PR opened from release/*": func(r *WorkflowRun) { r.Event, r.HeadBranch = "pull_request", "release/1.2" },
		"missing trigger":          func(r *WorkflowRun) { r.Event = "" },
		"failed run":               func(r *WorkflowRun) { r.Conclusion = "failure" },
		"cancelled run":            func(r *WorkflowRun) { r.Conclusion = "cancelled" },
		"still in progress":        func(r *WorkflowRun) { r.Status, r.Conclusion = "in_progress", "" },
		"feature branch":           func(r *WorkflowRun) { r.HeadBranch = "feature/cache" },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			h := NewWebhookHandler(testSecret, "", nil, nil)
			if rec := send(t, h, "workflow_run", run(mod)); rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 (acknowledged, ignored)", rec.Code)
			}
			if events := drain(h); len(events) != 0 {
				t.Fatalf("emitted %d event(s) for a non-deploy run: %+v", len(events), events[0])
			}
		})
	}
}

func TestWorkflowRun_AcceptedDeployTriggers(t *testing.T) {
	for _, trigger := range []string{"push", "workflow_dispatch", "repository_dispatch", "release", "workflow_run"} {
		h := NewWebhookHandler(testSecret, "", nil, nil)
		send(t, h, "workflow_run", run(func(r *WorkflowRun) { r.Event = trigger }))
		if len(drain(h)) != 1 {
			t.Errorf("trigger %q should count as a deploy", trigger)
		}
	}
}

func TestWorkflowRun_DeployWorkflowFilter(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		mod      func(*WorkflowRun)
		want     bool
	}{
		{"name match", []string{"Deploy"}, nil, true},
		{"name match is case-insensitive", []string{"deploy"}, nil, true},
		{"CI is not a deploy", []string{"Deploy"}, func(r *WorkflowRun) { r.Name, r.Path = "CI", ".github/workflows/ci.yml" }, false},
		{"glob on name", []string{"Deploy*"}, func(r *WorkflowRun) { r.Name = "Deploy to prod" }, true},
		{"match by file base name", []string{"release.yml"},
			func(r *WorkflowRun) { r.Name, r.Path = "Ship it", ".github/workflows/release.yml@refs/heads/main" }, true},
		{"match by full path", []string{".github/workflows/release.yml"},
			func(r *WorkflowRun) { r.Name, r.Path = "Ship it", ".github/workflows/release.yml" }, true},
		{"survives a renamed workflow when matching the path", []string{"deploy.yml"},
			func(r *WorkflowRun) { r.Name = "Totally different title" }, true},
		{"any of several patterns", []string{"Release", "Deploy"}, nil, true},
		{"blank patterns mean no filter", []string{"", "  "}, func(r *WorkflowRun) { r.Name = "CI" }, true},
		{"no patterns accepts everything", nil, func(r *WorkflowRun) { r.Name = "CI" }, true},
		{"malformed glob falls back to equality", []string{"[Deploy"}, nil, false},
		// `on: workflow_run: workflows: [CI]` — the commonest CI -> deploy chain.
		{"deploy workflow chained off CI", []string{"Deploy"}, func(r *WorkflowRun) { r.Event = "workflow_run" }, true},
		{"chained CI run is filtered out by name", []string{"Deploy"},
			func(r *WorkflowRun) { r.Event, r.Name, r.Path = "workflow_run", "CI", ".github/workflows/ci.yml" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewWebhookHandler(testSecret, "", tc.patterns, nil)
			send(t, h, "workflow_run", run(tc.mod))
			if got := len(drain(h)) == 1; got != tc.want {
				t.Errorf("deploy = %v, want %v", got, tc.want)
			}
		})
	}
}

func deploymentStatus(state, env, sha string) DeploymentStatusPayload {
	var p DeploymentStatusPayload
	p.DeploymentStatus.State = state
	p.DeploymentStatus.CreatedAt = runTime.Add(5 * time.Minute)
	p.Deployment.SHA, p.Deployment.Ref, p.Deployment.Environment = sha, "v1.4.0", env
	p.Repository.FullName = "Acme/Payments"
	return p
}

func TestDeploymentStatus_SuccessfulProductionDeploy(t *testing.T) {
	h := NewWebhookHandler(testSecret, "", nil, nil)
	if rec := send(t, h, "deployment_status", deploymentStatus("success", "production", "abc123")); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	events := drain(h)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Environment != "production" || ev.CommitSHA != "abc123" || ev.Branch != "v1.4.0" {
		t.Errorf("event = %+v", ev)
	}
	if !ev.OccurredAt.Equal(runTime.Add(5 * time.Minute)) {
		t.Errorf("OccurredAt = %v, want the status's created_at", ev.OccurredAt)
	}
	// Same commit as a workflow_run for it ⇒ the same deploy, merged by the store.
	if want := models.DeployEventID(models.DeploySourceGitHubActions, "Acme/Payments", "abc123"); ev.ID != want {
		t.Errorf("ID = %s, want %s (shared with workflow_run events for the commit)", ev.ID, want)
	}
}

func TestDeploymentStatus_Ignored(t *testing.T) {
	cases := map[string]DeploymentStatusPayload{
		"failure":                 deploymentStatus("failure", "production", "abc123"),
		"in_progress":             deploymentStatus("in_progress", "production", "abc123"),
		"staging":                 deploymentStatus("success", "staging", "abc123"),
		"preview":                 deploymentStatus("success", "pr-482-preview", "abc123"),
		"no environment":          deploymentStatus("success", "", "abc123"),
		"preprod":                 deploymentStatus("success", "preprod", "abc123"),
		"pre-production":          deploymentStatus("success", "pre-production", "abc123"),
		"non-prod":                deploymentStatus("success", "non-prod", "abc123"),
		"nonprod":                 deploymentStatus("success", "nonprod", "abc123"),
		"staging-production-like": deploymentStatus("success", "staging-production-like", "abc123"),
		"no commit":               deploymentStatus("success", "production", ""),
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			h := NewWebhookHandler(testSecret, "", nil, nil)
			if rec := send(t, h, "deployment_status", p); rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", rec.Code)
			}
			if events := drain(h); len(events) != 0 {
				t.Fatalf("emitted %d event(s), want none", len(events))
			}
		})
	}
}

// A full queue must answer promptly with 503 (so GitHub retries; deterministic
// IDs make that safe) and must never block the handler.
func TestFullQueueReturns503WithoutBlocking(t *testing.T) {
	h := NewWebhookHandler(testSecret, "", nil, nil)
	h.events = make(chan models.DeployEvent, 1)
	h.events <- models.DeployEvent{} // queue is now full

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- send(t, h, "workflow_run", run(nil)) }()

	select {
	case rec := <-done:
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Error("503 should carry Retry-After")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler blocked on a full queue")
	}
}

func TestWithoutTokenThereIsNoEnrichment(t *testing.T) {
	if h := NewWebhookHandler(testSecret, "", nil, nil); h.enricher != nil {
		t.Error("enricher created without a token: it would call the GitHub API with an empty bearer token")
	}
	if h := NewWebhookHandler(testSecret, "ghp_x", nil, nil); h.enricher == nil {
		t.Error("enricher missing despite a token")
	}
}

func TestMalformedSignedPayloadIs400(t *testing.T) {
	h := NewWebhookHandler(testSecret, "", nil, nil)
	if rec := sendRaw(h, "workflow_run", []byte(`{not json`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestBadSignatureIs401AndQueuesNothing(t *testing.T) {
	h := NewWebhookHandler(testSecret, "", nil, nil)
	body, _ := json.Marshal(run(nil))
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "workflow_run")
	req.Header.Set("X-Hub-Signature-256", sign([]byte("wrong"), body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized || len(drain(h)) != 0 {
		t.Fatalf("status = %d, want 401 with nothing queued", rec.Code)
	}
}

func TestPingIs200(t *testing.T) {
	h := NewWebhookHandler(testSecret, "", nil, nil)
	if rec := sendRaw(h, "ping", []byte(`{"zen":"x"}`)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestIsProductionEnvironment(t *testing.T) {
	for env, want := range map[string]bool{
		"production": true, "Production": true, "prod": true, "prod-eu": true, "prod_us_east": true,
		"prod2": true, "live": true, "eu-production": true,
		"": false, "staging": false, "preprod": false, "pre-production": false, "non-prod": false,
		"nonprod": false, "dev": false, "qa": false, "pr-482-preview": false, "product-docs": false,
	} {
		if got := isProductionEnvironment(env); got != want {
			t.Errorf("isProductionEnvironment(%q) = %v, want %v", env, got, want)
		}
	}
}

func mapFor(t *testing.T, entries ...correlate.ServiceMapEntry) *correlate.ServiceMap {
	t.Helper()
	sm, err := correlate.NewServiceMap(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sm
}

// Without a token nothing is fetched, but a repository rule still gives the
// deploy a service, so it can reach the alert threshold.
func TestRepoRuleGivesServicesWithoutAToken(t *testing.T) {
	h := NewWebhookHandler(testSecret, "", nil, mapFor(t, correlate.ServiceMapEntry{Match: "acme/payments", Services: []string{"AWS Lambda"}}))

	send(t, h, "workflow_run", run(nil)) // repository is Acme/Payments: matching ignores case
	events := drain(h)
	if len(events) != 1 || len(events[0].InferredServices) != 1 || events[0].InferredServices[0] != "AWS Lambda" {
		t.Fatalf("events = %+v, want one carrying [AWS Lambda]", events)
	}

	send(t, h, "deployment_status", deploymentStatus("success", "production", "abc123"))
	events = drain(h)
	if len(events) != 1 || len(events[0].InferredServices) != 1 {
		t.Errorf("deployment_status events get the repo rule too, got %+v", events)
	}
}

func TestRepoRuleDoesNotApplyToOtherRepositories(t *testing.T) {
	h := NewWebhookHandler(testSecret, "", nil, mapFor(t, correlate.ServiceMapEntry{Match: "acme/billing", Services: []string{"RDS"}}))
	send(t, h, "workflow_run", run(nil))
	if events := drain(h); len(events) != 1 || len(events[0].InferredServices) != 0 {
		t.Errorf("unrelated repo got services: %+v", events)
	}
}

// Enrichment adds file evidence on top of the repo rule instead of replacing it.
func TestEnrichmentUnionsFilesWithTheRepoRule(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/Acme/Payments/pulls/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "title": "feat: cache", "user": map[string]any{"login": "alice"}})
	})
	mux.HandleFunc("/repos/Acme/Payments/pulls/42/files", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"filename": "terraform/s3/bucket.tf"}, {"filename": "docs/readme.md"}})
	})
	api := httptest.NewServer(mux)
	defer api.Close()

	sm := mapFor(t, correlate.ServiceMapEntry{Match: "acme/payments", Services: []string{"DynamoDB"}})
	e := newEnricher("ghp_test", sm)
	e.baseURL = api.URL

	base := eventFromRun("Acme/Payments", run(nil).WorkflowRun)
	base.InferredServices = sm.ForKeys(base.Repository)

	got, err := e.Enrich(context.Background(), base, 42)
	if err != nil {
		t.Fatal(err)
	}
	if got.PRNumber != 42 || got.PRAuthor != "alice" || len(got.ChangedFiles) != 2 {
		t.Errorf("PR metadata not applied: %+v", got)
	}
	have := map[string]bool{}
	for _, s := range got.InferredServices {
		have[s] = true
	}
	if !have["DynamoDB"] || !have["AmazonS3"] || len(have) != 2 {
		t.Errorf("services = %v, want the rule's DynamoDB plus S3 from the changed files", got.InferredServices)
	}
}
