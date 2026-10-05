package gitlab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prem0x01/costblame/internal/correlate"
	"github.com/prem0x01/costblame/pkg/models"
)

// fakeGitLab serves the commit diff endpoint the receiver calls.
type fakeGitLab struct {
	t        *testing.T
	pages    [][]map[string]any // one slice of diffs per page
	status   int                // non-zero forces an error status
	calls    atomic.Int64
	gotToken atomic.Value // last PRIVATE-TOKEN header
	gotPath  atomic.Value // last request path
}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls.Add(1)
	f.gotToken.Store(r.Header.Get("PRIVATE-TOKEN"))
	f.gotPath.Store(r.URL.EscapedPath())
	if f.status != 0 {
		http.Error(w, "boom", f.status)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page < len(f.pages) {
		w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
	}
	if page > len(f.pages) {
		json.NewEncoder(w).Encode([]any{})
		return
	}
	json.NewEncoder(w).Encode(f.pages[page-1])
}

func diff(newPath string) map[string]any {
	return map[string]any{"new_path": newPath, "old_path": newPath}
}

func newAPI(t *testing.T, f *fakeGitLab) (*httptest.Server, Options) {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv, Options{Token: "glpat-test", BaseURL: srv.URL}
}

func pipelineBody(sha string, extra string) string {
	return `{"object_kind":"pipeline","object_attributes":{"status":"success","ref":"main","sha":"` + sha + `"},` +
		`"project":{"id":42,"path_with_namespace":"acme/payments"},"commit":{"timestamp":"2026-07-08T10:00:00Z"}` + extra + `}`
}

func twoEvents(t *testing.T, h *WebhookHandler, body string) (raw, enriched models.DeployEvent) {
	t.Helper()
	if rec := post(h, "tok", body); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	var ok bool
	if raw, ok = next(h, 3*time.Second); !ok {
		t.Fatal("no raw event")
	}
	if enriched, ok = next(h, 3*time.Second); !ok {
		t.Fatal("no enriched event")
	}
	return raw, enriched
}

// The point of the feature: a pipeline for a commit touching terraform/lambda/
// ends up with the services those files imply, so it can match a spike.
func TestEnrichment_InfersServicesFromTheCommitDiff(t *testing.T) {
	api := &fakeGitLab{t: t, pages: [][]map[string]any{{diff("terraform/lambda/main.tf"), diff("README.md")}}}
	_, opts := newAPI(t, api)
	h := New("tok", opts)

	raw, enriched := twoEvents(t, h, pipelineBody("abc123", ""))

	if len(raw.ChangedFiles) != 0 {
		t.Errorf("the raw event should be emitted before the API call, got files %v", raw.ChangedFiles)
	}
	if got := enriched.InferredServices; len(got) != 1 || got[0] != "AWSLambda" {
		t.Errorf("InferredServices = %v, want [AWSLambda]", got)
	}
	if len(enriched.ChangedFiles) != 2 {
		t.Errorf("ChangedFiles = %v", enriched.ChangedFiles)
	}
	if raw.ID != enriched.ID {
		t.Errorf("enrichment must reuse the deploy ID so the store merges it: %s vs %s", raw.ID, enriched.ID)
	}
	if got := api.gotToken.Load(); got != "glpat-test" {
		t.Errorf("PRIVATE-TOKEN header = %v", got)
	}
	// The project id from the payload, not the path, is used.
	if got := api.gotPath.Load().(string); !strings.Contains(got, "/api/v4/projects/42/repository/commits/abc123/diff") {
		t.Errorf("request path = %s", got)
	}
}

func TestEnrichment_FollowsPaginationAndUsesOldPathForDeletedFiles(t *testing.T) {
	api := &fakeGitLab{t: t, pages: [][]map[string]any{
		{diff("a.go"), diff("b.go")},
		{{"new_path": "terraform/s3/old.tf", "old_path": "terraform/s3/old.tf", "deleted_file": true}, diff("terraform/rds/db.tf")},
	}}
	_, opts := newAPI(t, api)
	_, enriched := twoEvents(t, New("tok", opts), pipelineBody("def456", ""))

	if len(enriched.ChangedFiles) != 4 {
		t.Errorf("ChangedFiles = %v, want all 4 across both pages", enriched.ChangedFiles)
	}
	have := map[string]bool{}
	for _, s := range enriched.InferredServices {
		have[s] = true
	}
	if !have["AmazonS3"] || !have["AmazonRDS"] {
		t.Errorf("InferredServices = %v, want S3 (from the deleted file's path) and RDS", enriched.InferredServices)
	}
	if got := api.calls.Load(); got != 2 {
		t.Errorf("API calls = %d, want 2 pages", got)
	}
}

func TestEnrichment_CombinesRepoRuleWithFileEvidence(t *testing.T) {
	sm, err := correlate.NewServiceMap([]correlate.ServiceMapEntry{{Match: "acme/payments", Services: []string{"DynamoDB"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeGitLab{t: t, pages: [][]map[string]any{{diff("terraform/lambda/main.tf")}}}
	_, opts := newAPI(t, api)
	opts.ServiceMap = sm
	raw, enriched := twoEvents(t, New("tok", opts), pipelineBody("abc123", ""))

	if len(raw.InferredServices) != 1 || raw.InferredServices[0] != "DynamoDB" {
		t.Errorf("the raw event should already carry the repo rule's services, got %v", raw.InferredServices)
	}
	if len(enriched.InferredServices) != 2 {
		t.Errorf("enriched services = %v, want the rule's DynamoDB plus Lambda from the files", enriched.InferredServices)
	}
}

func TestNoToken_NeverCallsTheAPIButStillAppliesRepoRules(t *testing.T) {
	sm, _ := correlate.NewServiceMap([]correlate.ServiceMapEntry{{Match: "acme/*", Services: []string{"AWS Lambda"}}}, nil)
	api := &fakeGitLab{t: t, pages: [][]map[string]any{{diff("terraform/s3/x.tf")}}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	h := New("tok", Options{BaseURL: srv.URL, ServiceMap: sm}) // no token

	post(h, "tok", pipelineBody("abc123", ""))
	raw, ok := next(h, 2*time.Second)
	if !ok {
		t.Fatal("no event")
	}
	if len(raw.InferredServices) != 1 || raw.InferredServices[0] != "AWS Lambda" {
		t.Errorf("repo rule not applied: %v", raw.InferredServices)
	}
	if _, more := next(h, 200*time.Millisecond); more {
		t.Error("a second (enriched) event appeared without a token")
	}
	if api.calls.Load() != 0 {
		t.Errorf("the API was called %d time(s) without a token", api.calls.Load())
	}
}

func TestEnrichmentFailureLeavesTheRawEventAlone(t *testing.T) {
	for name, status := range map[string]int{"unauthorized": 401, "not found": 404, "server error": 500} {
		t.Run(name, func(t *testing.T) {
			api := &fakeGitLab{t: t, status: status}
			_, opts := newAPI(t, api)
			h := New("tok", opts)

			post(h, "tok", pipelineBody("abc123", ""))
			if _, ok := next(h, 2*time.Second); !ok {
				t.Fatal("the raw event must still be emitted when the API fails")
			}
			if ev, more := next(h, 200*time.Millisecond); more {
				t.Errorf("an enriched event was emitted despite the API error: %+v", ev)
			}
		})
	}
}

func TestEmptyDiffEmitsNoSecondEvent(t *testing.T) {
	api := &fakeGitLab{t: t, pages: [][]map[string]any{{}}}
	_, opts := newAPI(t, api)
	h := New("tok", opts)
	post(h, "tok", pipelineBody("abc123", ""))
	next(h, 2*time.Second)
	if _, more := next(h, 200*time.Millisecond); more {
		t.Error("nothing to add, so no enriched event is expected")
	}
}

func TestAPIURLNormalisation(t *testing.T) {
	for in, want := range map[string]string{
		"":                                  "https://gitlab.com/api/v4",
		"https://gitlab.com":                "https://gitlab.com/api/v4",
		"https://gitlab.example.com/":       "https://gitlab.example.com/api/v4",
		"https://gitlab.example.com/api/v4": "https://gitlab.example.com/api/v4",
		" https://git.corp/gitlab/ ":        "https://git.corp/gitlab/api/v4",
	} {
		if got := apiURL(in); got != want {
			t.Errorf("apiURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// With no project id in the payload the URL-encoded path is used instead.
func TestEnrichmentFallsBackToTheProjectPath(t *testing.T) {
	api := &fakeGitLab{t: t, pages: [][]map[string]any{{diff("terraform/lambda/x.tf")}}}
	_, opts := newAPI(t, api)
	h := New("tok", opts)
	body := `{"object_kind":"pipeline","object_attributes":{"status":"success","ref":"main","sha":"abc"},"project":{"path_with_namespace":"acme/payments"}}`
	post(h, "tok", body)
	next(h, 2*time.Second)
	next(h, 2*time.Second)
	if got := api.gotPath.Load().(string); !strings.Contains(got, "/projects/acme%2Fpayments/") {
		t.Errorf("request path = %s, want the URL-encoded project path", got)
	}
}

func TestAuthorFallbacks(t *testing.T) {
	for name, tc := range map[string]struct {
		extra, want string
	}{
		"merge request author wins": {`,"merge_request":{"iid":5,"author":{"username":"mr-author"}},"user":{"username":"pusher"}`, "mr-author"},
		"pipeline user next":        {`,"user":{"username":"pusher"}`, "pusher"},
	} {
		h := New("tok", Options{})
		post(h, "tok", pipelineBody("abc123", tc.extra))
		ev, ok := next(h, 2*time.Second)
		if !ok || ev.PRAuthor != tc.want {
			t.Errorf("%s: author = %q (ok=%v), want %q", name, ev.PRAuthor, ok, tc.want)
		}
	}
	// commit author as the last resort
	h := New("tok", Options{})
	body := `{"object_kind":"pipeline","object_attributes":{"status":"success","ref":"main","sha":"abc"},"project":{"path_with_namespace":"acme/payments"},"commit":{"author":{"name":"Commit Author"}}}`
	post(h, "tok", body)
	if ev, ok := next(h, 2*time.Second); !ok || ev.PRAuthor != "Commit Author" {
		t.Errorf("last-resort author = %q", ev.PRAuthor)
	}
}
