package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/prem0x01/costblame/internal/correlate"
	"github.com/prem0x01/costblame/pkg/models"
)

// WebhookHandler receives GitHub webhook events and converts them to DeployEvents.
// It must be registered on the `workflow_run` and `deployment_status` event types
// in the GitHub webhook settings.
type WebhookHandler struct {
	secret   []byte
	events   chan models.DeployEvent
	enricher *enricher
}

// NewWebhookHandler creates a handler that validates HMAC signatures using secret
// and uses ghToken to fetch enrichment data (changed files, PR info) from GitHub API.
func NewWebhookHandler(secret, ghToken string) *WebhookHandler {
	return &WebhookHandler{
		secret:   []byte(secret),
		events:   make(chan models.DeployEvent, 256),
		enricher: newEnricher(ghToken),
	}
}

// Name implements collect.DeploySource.
func (h *WebhookHandler) Name() string { return string(models.DeploySourceGitHubActions) }

// Events implements collect.DeploySource — returns the internal channel.
// The channel is never closed; it stays open as long as the server runs.
func (h *WebhookHandler) Events(ctx context.Context) (<-chan models.DeployEvent, error) {
	return h.events, nil
}

// ServeHTTP handles POST /webhooks/github.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20)) // 2 MB cap
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	if !h.verifySignature(r.Header.Get("X-Hub-Signature-256"), body) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	eventType := r.Header.Get("X-GitHub-Event")
	switch eventType {
	case "workflow_run":
		h.handleWorkflowRun(r.Context(), body)
	case "ping":
		w.WriteHeader(http.StatusOK)
		return
	default:
		// Accept but ignore other event types GitHub may send.
	}

	w.WriteHeader(http.StatusAccepted)
}

func (h *WebhookHandler) handleWorkflowRun(ctx context.Context, body []byte) {
	var payload WorkflowRunPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		slog.Warn("github: failed to parse workflow_run payload", "err", err)
		return
	}

	// Only process completed successful runs; ignore in-progress or failed runs.
	if payload.WorkflowRun.Status != "completed" || payload.WorkflowRun.Conclusion != "success" {
		return
	}

	// Only consider runs on branches that look like production deployments.
	if !isDeployBranch(payload.WorkflowRun.HeadBranch) {
		return
	}

	event := models.DeployEvent{
		ID:          uuid.New(),
		OccurredAt:  payload.WorkflowRun.UpdatedAt,
		Source:      models.DeploySourceGitHubActions,
		Repository:  payload.Repository.FullName,
		Branch:      payload.WorkflowRun.HeadBranch,
		CommitSHA:   payload.WorkflowRun.HeadSHA,
		Environment: inferEnvironment(payload.WorkflowRun.HeadBranch),
		Status:      models.DeployStatusSuccess,
		RawPayload:  body,
	}

	// Acknowledge immediately; enrich asynchronously.
	h.events <- event

	// Enrichment fetches PR details and changed files from GitHub API.
	// The result is re-sent on the channel as an upsert — the store layer
	// merges enriched fields by ID.
	go h.enrichAsync(event, payload)
}

func (h *WebhookHandler) enrichAsync(event models.DeployEvent, payload WorkflowRunPayload) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	enriched, err := h.enricher.Enrich(ctx, event, payload)
	if err != nil {
		slog.Warn("github: enrichment failed", "repo", event.Repository, "commit", event.CommitSHA, "err", err)
		return
	}

	h.events <- *enriched
}

func (h *WebhookHandler) verifySignature(sigHeader string, body []byte) bool {
	if !strings.HasPrefix(sigHeader, "sha256=") {
		return false
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(sigHeader, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, h.secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), sig)
}

// isDeployBranch returns true for branch names that typically gate production deploys.
func isDeployBranch(branch string) bool {
	branch = strings.ToLower(branch)
	return branch == "main" ||
		branch == "master" ||
		strings.HasPrefix(branch, "release/") ||
		strings.HasPrefix(branch, "deploy/")
}

// inferEnvironment maps a branch name to an environment label.
func inferEnvironment(branch string) string {
	branch = strings.ToLower(branch)
	switch {
	case branch == "main" || branch == "master":
		return "prod"
	case strings.HasPrefix(branch, "release/"):
		return "prod"
	case strings.HasPrefix(branch, "staging"):
		return "staging"
	default:
		return "unknown"
	}
}

// enricher fetches PR metadata and changed files from the GitHub REST API.
type enricher struct {
	token  string
	client *http.Client
}

func newEnricher(token string) *enricher {
	return &enricher{
		token:  token,
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

func (e *enricher) Enrich(ctx context.Context, event models.DeployEvent, payload WorkflowRunPayload) (*models.DeployEvent, error) {
	// Use PR numbers embedded in the workflow_run payload if available.
	var prNumber int
	if len(payload.WorkflowRun.PullRequests) > 0 {
		prNumber = payload.WorkflowRun.PullRequests[0].Number
	} else {
		var err error
		prNumber, err = e.findPRForCommit(ctx, payload.Repository.FullName, payload.WorkflowRun.HeadSHA)
		if err != nil || prNumber == 0 {
			return &event, nil // no PR found — return unenriched event
		}
	}

	pr, err := e.fetchPR(ctx, payload.Repository.FullName, prNumber)
	if err != nil {
		return &event, fmt.Errorf("fetching PR: %w", err)
	}

	files, err := e.fetchChangedFiles(ctx, payload.Repository.FullName, prNumber)
	if err != nil {
		slog.Warn("github: failed to fetch changed files", "pr", prNumber, "err", err)
	}

	event.PRNumber = pr.Number
	event.PRTitle = pr.Title
	event.PRAuthor = pr.User.Login
	event.PRLabels = extractLabels(pr.Labels)
	event.ChangedFiles = files
	event.InferredServices = correlate.InferServicesFromFiles(files)

	return &event, nil
}

func (e *enricher) fetchPR(ctx context.Context, repo string, number int) (*PullRequest, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d", repo, number)
	var pr PullRequest
	if err := e.get(ctx, url, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

func (e *enricher) fetchChangedFiles(ctx context.Context, repo string, prNumber int) ([]string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d/files?per_page=100", repo, prNumber)
	var files []File
	if err := e.get(ctx, url, &files); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Filename)
	}
	return names, nil
}

func (e *enricher) findPRForCommit(ctx context.Context, repo, sha string) (int, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/commits/%s/pulls", repo, sha)
	var prs []PullRequest
	if err := e.get(ctx, url, &prs); err != nil {
		return 0, err
	}
	if len(prs) == 0 {
		return 0, nil
	}
	return prs[0].Number, nil
}

func (e *enricher) get(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("GitHub API %s: HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func extractLabels(labels []Label) []string {
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	return names
}
