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
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/prem0x01/costblame/internal/correlate"
	"github.com/prem0x01/costblame/pkg/models"
)

// WebhookHandler receives GitHub webhook events and converts them to DeployEvents.
// Register it for the `workflow_run` event and, optionally, `deployment_status`
// in the GitHub webhook settings.
//
// One commit is one deploy: every event kind and every workflow for a commit
// resolves to the same deterministic ID (see models.DeployEventID), and the
// store merges them. GitHub redeliveries are therefore idempotent.
type WebhookHandler struct {
	secret    []byte
	workflows []string // deploy workflow name/path patterns; empty = any workflow
	events    chan models.DeployEvent
	enricher  *enricher // nil without a token: PR enrichment is off
	sm        *correlate.ServiceMap
}

// NewWebhookHandler creates a handler that validates HMAC signatures using secret.
// A non-empty ghToken enables PR/changed-file enrichment via the GitHub API;
// without one, events are stored with no PR metadata. deployWorkflows restricts
// which workflows count as deploys (matched case-insensitively, with `*` globs,
// against the workflow's name and file path); empty accepts any workflow.
// serviceMap (may be nil) assigns services by repository, so deploys can match a
// spiking service even without a token, when no changed files are known.
func NewWebhookHandler(secret, ghToken string, deployWorkflows []string, serviceMap *correlate.ServiceMap) *WebhookHandler {
	h := &WebhookHandler{
		secret:    []byte(secret),
		workflows: cleanPatterns(deployWorkflows),
		events:    make(chan models.DeployEvent, 256),
		sm:        serviceMap,
	}
	if ghToken != "" {
		h.enricher = newEnricher(ghToken, serviceMap)
	}
	return h
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

	status := http.StatusAccepted // also for event types we deliberately ignore
	switch r.Header.Get("X-GitHub-Event") {
	case "workflow_run":
		status = h.handleWorkflowRun(body)
	case "deployment_status":
		status = h.handleDeploymentStatus(body)
	case "ping":
		status = http.StatusOK
	}

	switch status {
	case http.StatusServiceUnavailable:
		// The event was NOT queued. The delivery shows as failed in the webhook's
		// "Recent Deliveries"; redelivering it (UI or API) is safe because deploy
		// IDs are deterministic, so it cannot create a duplicate.
		w.Header().Set("Retry-After", "30")
		http.Error(w, "event queue full, retry later", status)
	case http.StatusBadRequest:
		http.Error(w, "malformed payload", status)
	default:
		w.WriteHeader(status)
	}
}

func (h *WebhookHandler) handleWorkflowRun(body []byte) int {
	var payload WorkflowRunPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		slog.Warn("github: failed to parse workflow_run payload", "err", err)
		return http.StatusBadRequest
	}

	run := payload.WorkflowRun
	// Only completed successful runs of something that is actually a deploy.
	if run.Status != "completed" || run.Conclusion != "success" || !isDeployRun(run, h.workflows) {
		return http.StatusAccepted
	}

	event := eventFromRun(payload.Repository.FullName, run)
	event.RawPayload = body
	return h.dispatch(event, prHint(run))
}

// handleDeploymentStatus handles GitHub's Deployments API: the canonical deploy
// signal for teams that use it. Only successful production deployments count —
// a staging deploy of the same commit must not set the deploy time, because
// events for one commit merge into one row.
func (h *WebhookHandler) handleDeploymentStatus(body []byte) int {
	var p DeploymentStatusPayload
	if err := json.Unmarshal(body, &p); err != nil {
		slog.Warn("github: failed to parse deployment_status payload", "err", err)
		return http.StatusBadRequest
	}

	if p.DeploymentStatus.State != "success" || p.Deployment.SHA == "" || p.Repository.FullName == "" ||
		!isProductionEnvironment(p.Deployment.Environment) {
		return http.StatusAccepted
	}

	occurredAt := p.DeploymentStatus.CreatedAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	event := models.DeployEvent{
		ID:          models.DeployEventID(models.DeploySourceGitHubActions, p.Repository.FullName, p.Deployment.SHA),
		OccurredAt:  occurredAt.UTC(),
		Source:      models.DeploySourceGitHubActions,
		Repository:  p.Repository.FullName,
		Branch:      p.Deployment.Ref,
		CommitSHA:   p.Deployment.SHA,
		Environment: p.Deployment.Environment,
		Status:      models.DeployStatusSuccess,
		RawPayload:  body,
	}
	return h.dispatch(event, 0)
}

// dispatch queues the event without blocking the webhook response and, when a
// token is configured, starts background PR enrichment. It returns 202, or 503
// when the queue is full.
func (h *WebhookHandler) dispatch(event models.DeployEvent, prHint int) int {
	// A repository rule gives a service match straight away, and without a token.
	event.InferredServices = correlate.UnionServices(event.InferredServices, h.sm.ForKeys(event.Repository))

	select {
	case h.events <- event:
	default:
		slog.Warn("github: event queue full, asking sender to retry",
			"repo", event.Repository, "commit", event.CommitSHA)
		return http.StatusServiceUnavailable
	}

	if h.enricher != nil {
		go h.enrichAsync(event, prHint)
	}
	return http.StatusAccepted
}

func (h *WebhookHandler) enrichAsync(event models.DeployEvent, prHint int) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	enriched, err := h.enricher.Enrich(ctx, event, prHint)
	if err != nil {
		slog.Warn("github: enrichment failed", "repo", event.Repository, "commit", event.CommitSHA, "err", err)
		return
	}
	if enriched.PRNumber == 0 {
		return // no PR found; nothing to add to the stored event
	}

	// The enriched copy has the same ID, so the store merges it into the
	// original row. Bounded by ctx so this goroutine can't block forever if
	// the consumer has stopped (shutdown).
	select {
	case h.events <- *enriched:
	case <-ctx.Done():
		slog.Warn("github: dropping enrichment, event queue stayed full", "repo", event.Repository, "commit", event.CommitSHA)
	}
}

// eventFromRun builds the (unenriched) DeployEvent for a workflow run. The ID
// depends only on repository and commit, never on the delivery or the run.
func eventFromRun(repo string, run WorkflowRun) models.DeployEvent {
	return models.DeployEvent{
		ID:          models.DeployEventID(models.DeploySourceGitHubActions, repo, run.HeadSHA),
		OccurredAt:  run.UpdatedAt.UTC(),
		Source:      models.DeploySourceGitHubActions,
		Repository:  repo,
		Branch:      run.HeadBranch,
		CommitSHA:   run.HeadSHA,
		Environment: inferEnvironment(run.HeadBranch),
		Status:      models.DeployStatusSuccess,
	}
}

func prHint(run WorkflowRun) int {
	if len(run.PullRequests) > 0 {
		return run.PullRequests[0].Number
	}
	return 0
}

// deployTriggers are the workflow triggers that can mean "new code reached
// production". Everything else — notably `schedule` and the pull_request
// family — is excluded: scheduled runs execute against whatever HEAD is,
// possibly days old, and PR runs are not deploys at all.
//
// "workflow_run" is included on purpose: a deploy workflow chained off CI
// (`on: workflow_run: workflows: [CI]`) reports exactly that trigger. Without it
// the commonest CI→deploy layout would record no deploys at all. It is safe
// because the branch filter still applies and the store keeps the earliest time.
var deployTriggers = map[string]bool{
	"push":                true,
	"workflow_dispatch":   true,
	"repository_dispatch": true,
	"release":             true,
	"workflow_run":        true,
}

// isDeployRun reports whether a (completed, successful) workflow run should
// count as a deploy: a deploy-like trigger, a production branch, and, when
// patterns are configured, a matching workflow name or file path.
func isDeployRun(run WorkflowRun, patterns []string) bool {
	return deployTriggers[run.Event] &&
		isDeployBranch(run.HeadBranch) &&
		matchesWorkflow(run, patterns)
}

// matchesWorkflow reports whether run's workflow name or file path (full path
// or base name) matches any pattern. No patterns means every workflow matches.
func matchesWorkflow(run WorkflowRun, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	file := run.Path
	if i := strings.IndexByte(file, '@'); i >= 0 {
		file = file[:i] // strip the "@refs/heads/main" suffix
	}
	candidates := []string{run.Name, file, path.Base(file)}
	for _, pat := range patterns {
		for _, c := range candidates {
			if c != "" && globFold(pat, c) {
				return true
			}
		}
	}
	return false
}

// globFold is a case-insensitive path.Match that falls back to plain equality
// for a malformed pattern.
func globFold(pattern, name string) bool {
	pattern, name = strings.ToLower(pattern), strings.ToLower(name)
	if ok, err := path.Match(pattern, name); err == nil {
		return ok
	}
	return pattern == name
}

func cleanPatterns(in []string) []string {
	var out []string
	for _, p := range in {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// isProductionEnvironment reports whether a Deployments API environment name
// looks like production: "production", "prod", "prod-eu", "prod2", "live".
// It works on whole words, so "preprod", "pre-production", "non-prod" and
// "nonprod" — all staging in practice — are rejected even though they contain
// "prod".
func isProductionEnvironment(env string) bool {
	isProd := false
	for _, w := range strings.FieldsFunc(strings.ToLower(env), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if (strings.HasPrefix(w, "pre") || strings.HasPrefix(w, "non")) && strings.Contains(w, "prod") {
			return false // "preprod", "nonprod", "preproduction"
		}
		switch w {
		case "pre", "non", "staging", "stage", "stg", "dev", "test", "qa", "uat", "sandbox", "preview":
			return false
		case "prod", "production", "live":
			isProd = true
		default:
			if rest, ok := strings.CutPrefix(w, "prod"); ok && isDigits(rest) {
				isProd = true // "prod1", "prod2"
			}
		}
	}
	return isProd
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (h *WebhookHandler) verifySignature(sigHeader string, body []byte) bool {
	// Never accept unsigned traffic: an HMAC keyed with an empty secret is
	// computable by anyone, so an empty secret must fail closed.
	if len(h.secret) == 0 {
		return false
	}
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
	token   string
	client  *http.Client
	baseURL string // GitHub API root; overridable for tests and GitHub Enterprise
	sm      *correlate.ServiceMap
}

func newEnricher(token string, sm *correlate.ServiceMap) *enricher {
	return &enricher{
		token:   token,
		client:  &http.Client{Timeout: 20 * time.Second},
		baseURL: "https://api.github.com",
		sm:      sm,
	}
}

// Enrich fills in PR metadata and changed files for event's commit. prHint is a
// PR number already known from the payload (0 = look it up by commit). When no
// PR can be found the event is returned unchanged (PRNumber == 0).
func (e *enricher) Enrich(ctx context.Context, event models.DeployEvent, prHint int) (*models.DeployEvent, error) {
	repo, sha := event.Repository, event.CommitSHA

	prNumber := prHint
	if prNumber == 0 {
		var err error
		prNumber, err = e.findPRForCommit(ctx, repo, sha)
		if err != nil || prNumber == 0 {
			return &event, nil // no PR found — return unenriched event
		}
	}

	pr, err := e.fetchPR(ctx, repo, prNumber)
	if err != nil {
		return &event, fmt.Errorf("fetching PR: %w", err)
	}

	files, err := e.fetchChangedFiles(ctx, repo, prNumber)
	if err != nil {
		slog.Warn("github: failed to fetch changed files", "pr", prNumber, "err", err)
	}

	event.PRNumber = pr.Number
	event.PRTitle = pr.Title
	event.PRAuthor = pr.User.Login
	event.PRLabels = extractLabels(pr.Labels)
	event.ChangedFiles = files
	// Union with what the event already carries (a repository rule): evidence
	// from files adds to it.
	event.InferredServices = correlate.UnionServices(event.InferredServices, e.sm.InferFromFiles(files))

	return &event, nil
}

func (e *enricher) fetchPR(ctx context.Context, repo string, number int) (*PullRequest, error) {
	url := fmt.Sprintf("%s/repos/%s/pulls/%d", e.baseURL, repo, number)
	var pr PullRequest
	if err := e.get(ctx, url, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

func (e *enricher) fetchChangedFiles(ctx context.Context, repo string, prNumber int) ([]string, error) {
	url := fmt.Sprintf("%s/repos/%s/pulls/%d/files?per_page=100", e.baseURL, repo, prNumber)
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
	url := fmt.Sprintf("%s/repos/%s/commits/%s/pulls", e.baseURL, repo, sha)
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
