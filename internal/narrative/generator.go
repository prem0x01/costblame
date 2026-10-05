// Package narrative generates human-readable blame narratives from scored
// BlameEdge records. Generator is the built-in adapter for the Anthropic API;
// any other LLM provider can be plugged in by implementing
// correlate.NarrativeGenerator. NoopGenerator provides template narratives
// when no LLM is configured.
package narrative

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/prem0x01/costblame/pkg/models"
)

// Generator wraps the Anthropic client and generates blame narratives.
type Generator struct {
	client *anthropic.Client
	model  string
}

// New creates a Generator. model should be a valid Anthropic model ID;
// if empty it defaults to claude-sonnet-4-6.
func New(apiKey, model string) *Generator {
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	client := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &Generator{client: client, model: model}
}

// Generate produces a 2–3 sentence blame narrative for the given edge.
// The edge must have CostSnapshot and DeployEvent populated (denormalized fields).
func (g *Generator) Generate(ctx context.Context, edge models.BlameEdge) (string, error) {
	if edge.CostSnapshot == nil || edge.DeployEvent == nil {
		return "", fmt.Errorf("narrative: edge %s has nil denormalized fields — hydrate before generating", edge.ID)
	}

	bc := BuildContext(edge)
	systemPrompt, userPrompt := BuildPrompt(bc)

	msg, err := g.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.F(anthropic.Model(g.model)),
		MaxTokens: anthropic.F(int64(350)),
		System: anthropic.F([]anthropic.TextBlockParam{
			anthropic.NewTextBlock(systemPrompt),
		}),
		Messages: anthropic.F([]anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userPrompt)),
		}),
	})
	if err != nil {
		return "", fmt.Errorf("narrative: Anthropic API error: %w", err)
	}

	for _, block := range msg.Content {
		if block.Type == "text" {
			return block.Text, nil
		}
	}
	return "", fmt.Errorf("narrative: no text block in Anthropic API response")
}

// NoopGenerator is a drop-in replacement for Generator that returns a templated
// narrative without calling any LLM. Used when no provider is configured.
type NoopGenerator struct{}

func (n *NoopGenerator) Generate(_ context.Context, edge models.BlameEdge) (string, error) {
	if edge.CostSnapshot == nil || edge.DeployEvent == nil {
		return "(narrative unavailable — no LLM configured)", nil
	}
	snap := edge.CostSnapshot
	deploy := edge.DeployEvent
	return fmt.Sprintf(
		"Cost for %s increased %.1f%% (%.2f USD → %.2f USD) during the period starting %s. "+
			"PR #%d (%q by @%s) deployed %s and is the most likely cause "+
			"with a confidence score of %.0f%%. Review the changes and consider rolling back if the increase was unintended.",
		snap.Service, snap.DeltaPct, snap.PrevAmountUSD, snap.AmountUSD,
		snap.PeriodStart.Format("Jan 2 15:04 UTC"),
		deploy.PRNumber, deploy.PRTitle, deploy.PRAuthor,
		deployTiming(snap.PeriodStart, deploy.OccurredAt),
		edge.ConfidenceScore*100,
	), nil
}
