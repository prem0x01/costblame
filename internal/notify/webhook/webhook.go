// Package webhook implements a generic outbound webhook notifier.
// It POSTs a JSON payload to a configured URL whenever a high-confidence
// blame edge is identified.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/prem0x01/costblame/pkg/models"
)

// Notifier posts blame edges as JSON to an outbound HTTP endpoint.
type Notifier struct {
	url      string
	minScore float64
	client   *http.Client
}

// New creates a Notifier that POSTs to url when confidence ≥ minScore.
func New(url string, minScore float64) *Notifier {
	return &Notifier{
		url:      url,
		minScore: minScore,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

type payload struct {
	EdgeID     string  `json:"edge_id"`
	Service    string  `json:"service"`
	DeltaPct   float64 `json:"delta_pct"`
	AmountUSD  float64 `json:"amount_usd"`
	PRNumber   int     `json:"pr_number"`
	PRAuthor   string  `json:"pr_author"`
	Score      float64 `json:"confidence_score"`
	Narrative  string  `json:"narrative"`
	DetectedAt string  `json:"detected_at"`
}

// Send posts the graph's top blame edge to the configured webhook URL.
// It is a no-op when the graph has no edges or the top confidence is below
// the configured minimum.
func (n *Notifier) Send(ctx context.Context, graph models.BlameGraph) error {
	if graph.TopBlame == nil {
		return nil
	}
	edge := *graph.TopBlame
	if edge.CostSnapshot == nil {
		edge.CostSnapshot = &graph.Anomaly
	}
	return n.notify(ctx, edge)
}

// notify sends edge to the configured webhook URL.
func (n *Notifier) notify(ctx context.Context, edge models.BlameEdge) error {
	if edge.ConfidenceScore < n.minScore {
		return nil
	}

	p := payload{
		EdgeID:     edge.ID.String(),
		Score:      edge.ConfidenceScore,
		Narrative:  edge.Narrative,
		DetectedAt: edge.CreatedAt.UTC().Format(time.RFC3339),
	}
	if edge.CostSnapshot != nil {
		p.Service = edge.CostSnapshot.Service
		p.DeltaPct = edge.CostSnapshot.DeltaPct
		p.AmountUSD = edge.CostSnapshot.AmountUSD
	}
	if edge.DeployEvent != nil {
		p.PRNumber = edge.DeployEvent.PRNumber
		p.PRAuthor = edge.DeployEvent.PRAuthor
	}

	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("webhook: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: POST %s: %w", n.url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook: POST %s returned %d", n.url, resp.StatusCode)
	}
	return nil
}
