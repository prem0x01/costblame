// Package gitlab provides a deploy-event source backed by GitLab CI webhooks.
// This is a stub — wire it to an HTTP server the same way as the GitHub
// WebhookHandler and it will parse pipeline events into DeployEvents.
package gitlab

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prem0x01/costblame/internal/correlate"
	"github.com/prem0x01/costblame/pkg/models"
)

// Options configures optional behaviour of the GitLab receiver.
type Options struct {
	// Token is a GitLab access token with read_api scope. When set, each deploy's
	// changed files are fetched from the commit diff so services can be inferred
	// from them; without it the deploy carries no file list.
	Token string
	// BaseURL is the GitLab root (default https://gitlab.com); the /api/v4 suffix
	// is added if missing.
	BaseURL string
	// ServiceMap assigns services by repository, so deploys can match a service
	// even without a file list. Nil means the built-in path patterns only.
	ServiceMap *correlate.ServiceMap
	// HTTPClient overrides the client used for API calls (tests).
	HTTPClient *http.Client
}

// WebhookHandler receives GitLab pipeline webhooks and emits DeployEvents.
type WebhookHandler struct {
	secret string
	ch     chan models.DeployEvent
	token  string
	apiURL string
	sm     *correlate.ServiceMap
	client *http.Client
}

// New creates a WebhookHandler that validates the X-Gitlab-Token header.
func New(secret string, opts Options) *WebhookHandler {
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &WebhookHandler{
		secret: secret,
		ch:     make(chan models.DeployEvent, 256),
		token:  opts.Token,
		apiURL: apiURL(opts.BaseURL),
		sm:     opts.ServiceMap,
		client: client,
	}
}

// apiURL normalises a GitLab root URL to its REST API base.
func apiURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = "https://gitlab.com"
	}
	if !strings.HasSuffix(base, "/api/v4") {
		base += "/api/v4"
	}
	return base
}

// Name implements collect.DeploySource.
func (h *WebhookHandler) Name() string { return "gitlab" }

// Events implements collect.DeploySource.
func (h *WebhookHandler) Events(_ context.Context) (<-chan models.DeployEvent, error) {
	return h.ch, nil
}

// ServeHTTP handles incoming GitLab webhook requests.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	token := r.Header.Get("X-Gitlab-Token")
	// An empty secret fails closed: it would accept any request that omits the header.
	if h.secret == "" || subtle.ConstantTimeCompare([]byte(token), []byte(h.secret)) == 0 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	w.WriteHeader(http.StatusAccepted) // ack before heavy work

	go func() {
		if err := h.parse(body); err != nil {
			slog.Error("gitlab: parse webhook", "err", err)
		}
	}()
}

// pipelineEvent is the subset of GitLab's pipeline webhook payload we care about.
type pipelineEvent struct {
	ObjectKind       string `json:"object_kind"`
	ObjectAttributes struct {
		Status string `json:"status"`
		Ref    string `json:"ref"`
		SHA    string `json:"sha"`
	} `json:"object_attributes"`
	User struct {
		Username string `json:"username"`
	} `json:"user"`
	Project struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
		WebURL            string `json:"web_url"`
	} `json:"project"`
	MergeRequest struct {
		IID    int    `json:"iid"`
		Title  string `json:"title"`
		Author struct {
			Username string `json:"username"`
		} `json:"author"`
	} `json:"merge_request"`
	Commit struct {
		Timestamp string `json:"timestamp"`
		Author    struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"commit"`
}

func (h *WebhookHandler) parse(body []byte) error {
	var ev pipelineEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	if ev.ObjectKind != "pipeline" {
		return nil
	}
	if ev.ObjectAttributes.Status != "success" {
		return nil
	}
	if !isDeployBranch(ev.ObjectAttributes.Ref) {
		return nil
	}
	if ev.ObjectAttributes.SHA == "" {
		return nil // no commit to key the deploy on
	}

	occurredAt := time.Now().UTC()
	if ts, err := time.Parse(time.RFC3339, ev.Commit.Timestamp); err == nil {
		occurredAt = ts
	}

	deploy := models.DeployEvent{
		// One deploy per (project, commit): pipeline retries and several
		// pipelines for one commit merge instead of duplicating.
		ID:          models.DeployEventID(models.DeploySourceGitLabCI, ev.Project.PathWithNamespace, ev.ObjectAttributes.SHA),
		OccurredAt:  occurredAt.UTC(),
		Source:      models.DeploySourceGitLabCI,
		Repository:  ev.Project.PathWithNamespace,
		Branch:      ev.ObjectAttributes.Ref,
		CommitSHA:   ev.ObjectAttributes.SHA,
		PRNumber:    ev.MergeRequest.IID,
		PRTitle:     ev.MergeRequest.Title,
		PRAuthor:    author(ev),
		Status:      models.DeployStatusSuccess,
		RawPayload:  body,
		Environment: "production",
	}
	// A repository rule gives a service match even before (or without) the file list.
	deploy.InferredServices = h.sm.ForKeys(deploy.Repository)

	select {
	case h.ch <- deploy:
	default:
		slog.Warn("gitlab: event channel full, dropping deploy event", "repo", deploy.Repository)
		return nil
	}

	if h.token != "" {
		h.enrich(deploy, ev.Project.ID)
	}
	return nil
}

// author names the person to blame: the merge request's author, else whoever
// triggered the pipeline, else the commit's author. GitLab sends no merge
// request for plain pushes to a deploy branch, so the fallbacks matter.
func author(ev pipelineEvent) string {
	for _, name := range []string{ev.MergeRequest.Author.Username, ev.User.Username, ev.Commit.Author.Name} {
		if name != "" {
			return name
		}
	}
	return ""
}

// enrich fetches the commit's changed files from the GitLab API and re-emits the
// deploy with them and the services they imply. The enriched event has the same
// ID, so the store merges it into the row already written.
func (h *WebhookHandler) enrich(deploy models.DeployEvent, projectID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	files, err := h.changedFiles(ctx, projectID, deploy.Repository, deploy.CommitSHA)
	if err != nil {
		slog.Warn("gitlab: could not fetch changed files; service match will rely on service_map",
			"repo", deploy.Repository, "commit", deploy.CommitSHA, "err", err)
		return
	}
	if len(files) == 0 {
		return
	}
	deploy.ChangedFiles = files
	deploy.InferredServices = correlate.UnionServices(deploy.InferredServices, h.sm.InferFromFiles(files))

	select {
	case h.ch <- deploy:
	case <-ctx.Done():
		slog.Warn("gitlab: dropping enrichment, event queue stayed full", "repo", deploy.Repository)
	}
}

const (
	diffPageSize = 100
	maxDiffPages = 10 // at most 1000 files; a deploy that large is not a service-level signal
)

// changedFiles lists the paths changed by a commit. For a merge commit GitLab
// diffs against the first parent, i.e. everything the merge brought in.
func (h *WebhookHandler) changedFiles(ctx context.Context, projectID int64, projectPath, sha string) ([]string, error) {
	id := url.PathEscape(projectPath) // GitLab accepts a URL-encoded path as the project id
	if projectID > 0 {
		id = strconv.FormatInt(projectID, 10)
	}

	var files []string
	for page := 1; page <= maxDiffPages; page++ {
		endpoint := fmt.Sprintf("%s/projects/%s/repository/commits/%s/diff?per_page=%d&page=%d",
			h.apiURL, id, url.PathEscape(sha), diffPageSize, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("PRIVATE-TOKEN", h.token)

		resp, err := h.client.Do(req)
		if err != nil {
			return nil, err
		}
		var diffs []struct {
			NewPath     string `json:"new_path"`
			OldPath     string `json:"old_path"`
			DeletedFile bool   `json:"deleted_file"`
		}
		err = func() error {
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				return fmt.Errorf("GitLab API %s: HTTP %d", endpoint, resp.StatusCode)
			}
			return json.NewDecoder(resp.Body).Decode(&diffs)
		}()
		if err != nil {
			return nil, err
		}
		for _, d := range diffs {
			if d.DeletedFile {
				files = append(files, d.OldPath)
			} else {
				files = append(files, d.NewPath)
			}
		}
		if resp.Header.Get("X-Next-Page") == "" {
			break
		}
	}
	return files, nil
}

func isDeployBranch(ref string) bool {
	switch ref {
	case "main", "master":
		return true
	}
	for _, pfx := range []string{"release/", "deploy/"} {
		if len(ref) > len(pfx) && ref[:len(pfx)] == pfx {
			return true
		}
	}
	return false
}
