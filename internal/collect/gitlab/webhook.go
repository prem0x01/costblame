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
	"time"

	"github.com/google/uuid"

	"github.com/prem0x01/costblame/internal/correlate"
	"github.com/prem0x01/costblame/pkg/models"
)

// WebhookHandler receives GitLab pipeline webhooks and emits DeployEvents.
type WebhookHandler struct {
	secret string
	ch     chan models.DeployEvent
}

// New creates a WebhookHandler that validates the X-Gitlab-Token header.
func New(secret string) *WebhookHandler {
	return &WebhookHandler{
		secret: secret,
		ch:     make(chan models.DeployEvent, 256),
	}
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
	if h.secret != "" && subtle.ConstantTimeCompare([]byte(token), []byte(h.secret)) == 0 {
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
	Project struct {
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

	occurredAt := time.Now().UTC()
	if ts, err := time.Parse(time.RFC3339, ev.Commit.Timestamp); err == nil {
		occurredAt = ts
	}

	deploy := models.DeployEvent{
		ID:          uuid.New(),
		OccurredAt:  occurredAt,
		Source:      models.DeploySourceGitLabCI,
		Repository:  ev.Project.PathWithNamespace,
		Branch:      ev.ObjectAttributes.Ref,
		CommitSHA:   ev.ObjectAttributes.SHA,
		PRNumber:    ev.MergeRequest.IID,
		PRTitle:     ev.MergeRequest.Title,
		PRAuthor:    ev.MergeRequest.Author.Username,
		Status:      models.DeployStatusSuccess,
		RawPayload:  body,
		Environment: "production",
	}
	deploy.InferredServices = correlate.InferServicesFromFiles(nil)

	select {
	case h.ch <- deploy:
	default:
		slog.Warn("gitlab: event channel full, dropping deploy event", "repo", deploy.Repository)
	}
	return nil
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
