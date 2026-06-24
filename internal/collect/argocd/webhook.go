// Package argocd provides a deploy-event source backed by ArgoCD webhooks.
// ArgoCD fires a webhook when an application sync completes. Wire this handler
// to your HTTP server at (e.g.) POST /webhooks/argocd.
package argocd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/prem0x01/costblame/pkg/models"
)

// WebhookHandler receives ArgoCD application webhook events and emits DeployEvents.
type WebhookHandler struct {
	ch chan models.DeployEvent
}

// New creates a WebhookHandler for ArgoCD application events.
func New() *WebhookHandler {
	return &WebhookHandler{ch: make(chan models.DeployEvent, 256)}
}

// Name implements collect.DeploySource.
func (h *WebhookHandler) Name() string { return "argocd" }

// Events implements collect.DeploySource.
func (h *WebhookHandler) Events(_ context.Context) (<-chan models.DeployEvent, error) {
	return h.ch, nil
}

// ServeHTTP handles incoming ArgoCD application webhook requests.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	w.WriteHeader(http.StatusAccepted)

	go func() {
		if err := h.parse(body); err != nil {
			slog.Error("argocd: parse webhook", "err", err)
		}
	}()
}

// appEvent is the subset of the ArgoCD webhook payload we care about.
// ArgoCD sends a POST with the Application resource as JSON.
type appEvent struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Source struct {
			RepoURL        string `json:"repoURL"`
			TargetRevision string `json:"targetRevision"`
		} `json:"source"`
		Destination struct {
			Server    string `json:"server"`
			Namespace string `json:"namespace"`
		} `json:"destination"`
	} `json:"spec"`
	Status struct {
		Sync struct {
			Status   string `json:"status"`
			Revision string `json:"revision"`
		} `json:"sync"`
		OperationState struct {
			Phase     string `json:"phase"`
			FinishedAt string `json:"finishedAt"`
		} `json:"operationState"`
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
	} `json:"status"`
}

func (h *WebhookHandler) parse(body []byte) error {
	var ev appEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}

	if ev.Status.OperationState.Phase != "Succeeded" {
		return nil
	}

	occurredAt := time.Now().UTC()
	if ts, err := time.Parse(time.RFC3339, ev.Status.OperationState.FinishedAt); err == nil {
		occurredAt = ts
	}

	deploy := models.DeployEvent{
		ID:          uuid.New(),
		OccurredAt:  occurredAt,
		Source:      models.DeploySourceArgoCD,
		Repository:  ev.Spec.Source.RepoURL,
		Branch:      ev.Spec.Source.TargetRevision,
		CommitSHA:   ev.Status.Sync.Revision,
		Environment: ev.Spec.Destination.Namespace,
		Status:      models.DeployStatusSuccess,
		RawPayload:  body,
	}

	select {
	case h.ch <- deploy:
	default:
		slog.Warn("argocd: event channel full, dropping deploy event",
			"app", ev.Metadata.Name)
	}
	return nil
}
