// Package github provides both a webhook receiver and an API poller for
// GitHub Actions deployment events. The poller is a fallback for environments
// where inbound webhooks are not feasible (e.g. private networks).
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prem0x01/costblame/pkg/models"
)

// Poller periodically queries the GitHub Actions API for recent successful
// workflow runs and emits them as DeployEvents.
type Poller struct {
	token        string
	repos        []string // "owner/repo" pairs to poll
	pollInterval time.Duration
	lastSeen     map[string]time.Time // repo → last processed run time
	events       chan models.DeployEvent
	client       *http.Client
}

// NewPoller creates a poller for the given repos.
func NewPoller(token string, repos []string, pollInterval time.Duration) *Poller {
	return &Poller{
		token:        token,
		repos:        repos,
		pollInterval: pollInterval,
		lastSeen:     make(map[string]time.Time),
		events:       make(chan models.DeployEvent, 256),
		client:       &http.Client{Timeout: 20 * time.Second},
	}
}

func (p *Poller) Name() string { return string(models.DeploySourceGitHubActions) }

func (p *Poller) Events(ctx context.Context) (<-chan models.DeployEvent, error) {
	go p.loop(ctx)
	return p.events, nil
}

func (p *Poller) loop(ctx context.Context) {
	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()
	// Poll immediately on start, then on each tick.
	p.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

func (p *Poller) poll(ctx context.Context) {
	for _, repo := range p.repos {
		runs, err := p.fetchRecentRuns(ctx, repo)
		if err != nil {
			slog.Warn("github poller: fetch failed", "repo", repo, "err", err)
			continue
		}
		cutoff := p.lastSeen[repo]
		var newest time.Time
		for _, run := range runs {
			if run.Status != "completed" || run.Conclusion != "success" {
				continue
			}
			if !isDeployRun(run, nil) {
				continue
			}
			if !run.UpdatedAt.After(cutoff) {
				continue
			}
			if run.UpdatedAt.After(newest) {
				newest = run.UpdatedAt
			}
			event := eventFromRun(repo, run)
			raw, _ := json.Marshal(run)
			event.RawPayload = raw

			// Enrich inline for the poller — latency is less critical.
			e := newEnricher(p.token, nil)
			enriched, err := e.Enrich(ctx, event, prHint(run))
			if err != nil {
				slog.Warn("github poller: enrichment failed", "err", err)
				enriched = &event
			}
			select {
			case p.events <- *enriched:
			case <-ctx.Done():
				return
			}
		}
		if !newest.IsZero() {
			p.lastSeen[repo] = newest
		}
	}
}

type workflowRunsResponse struct {
	WorkflowRuns []WorkflowRun `json:"workflow_runs"`
}

func (p *Poller) fetchRecentRuns(ctx context.Context, repo string) ([]WorkflowRun, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/actions/runs?per_page=25&status=success", repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GitHub API: HTTP %d", resp.StatusCode)
	}

	var result workflowRunsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.WorkflowRuns, nil
}
