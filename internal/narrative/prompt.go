package narrative

import (
	"fmt"
	"strings"
	"time"

	"github.com/prem0x01/costblame/pkg/models"
)

// BlameContext is a flattened view of a BlameEdge used to render prompts.
// It avoids threading nested structs into the template functions.
type BlameContext struct {
	// Cost anomaly fields
	Service       string
	PeriodStart   time.Time
	PeriodEnd     time.Time
	AmountUSD     float64
	PrevAmountUSD float64
	DeltaPct      float64
	AnomalyScore  float64
	Tags          map[string]string

	// Deployment fields
	PRNumber     int
	PRTitle      string
	PRAuthor     string
	PRTeam       string
	DeployTime   time.Time
	ChangedFiles []string
	Environment  string

	// Correlation metadata
	ConfidenceScore   float64
	ConfidenceFactors []models.ConfidenceFactor
	HoursApart        float64
}

// BuildContext converts a BlameEdge (with embedded CostSnapshot and DeployEvent)
// into a BlameContext for prompt rendering. Panics if denormalized fields are nil —
// callers must hydrate the edge before generating narratives.
func BuildContext(edge models.BlameEdge) BlameContext {
	snap := edge.CostSnapshot
	deploy := edge.DeployEvent

	return BlameContext{
		Service:           snap.Service,
		PeriodStart:       snap.PeriodStart,
		PeriodEnd:         snap.PeriodEnd,
		AmountUSD:         snap.AmountUSD,
		PrevAmountUSD:     snap.PrevAmountUSD,
		DeltaPct:          snap.DeltaPct,
		AnomalyScore:      snap.AnomalyScore,
		Tags:              snap.Tags,
		PRNumber:          deploy.PRNumber,
		PRTitle:           deploy.PRTitle,
		PRAuthor:          deploy.PRAuthor,
		PRTeam:            deploy.PRTeam,
		DeployTime:        deploy.OccurredAt,
		ChangedFiles:      deploy.ChangedFiles,
		Environment:       deploy.Environment,
		ConfidenceScore:   edge.ConfidenceScore,
		ConfidenceFactors: edge.ConfidenceFactors,
		HoursApart:        snap.PeriodStart.Sub(deploy.OccurredAt).Hours(),
	}
}

// BuildPrompt renders the system and user prompts sent to the LLM.
// It returns (systemPrompt, userPrompt) as separate strings so callers can
// set them in the correct Anthropic API fields.
func BuildPrompt(bc BlameContext) (system, user string) {
	system = `You are a FinOps engineer at a fast-growing technology company.
Your job is to explain cloud cost anomalies to engineering teams in plain, factual English.
You write concisely and avoid jargon. You are non-accusatory — cost spikes happen, and
the goal is understanding and remediation, not blame. You never speculate beyond the data provided.`

	user = fmt.Sprintf(`A cloud cost anomaly has been detected. Based on the data below, write a 2–3 sentence
explanation suitable for a Slack alert. The message should:
1. State what service spiked and by how much, with the time window.
2. Name the most likely deployment cause and describe what it changed.
3. Suggest one concrete next action for the team.

Start your response with exactly: "Cost for"
Use no bullet points. Do not mention confidence scores. Do not use dollar signs — write amounts as USD values.

COST ANOMALY
  Service    : %s
  Period     : %s → %s
  Amount     : $%.2f (was $%.2f, +%.1f%%)
  Anomaly    : %.1f deviations above the 30-day median (MAD-scaled, comparable to standard deviations)
  Tags       : %s

MOST LIKELY DEPLOYMENT
  PR #%d     : "%s"
  Author     : @%s (%s team)
  Deployed   : %s (%s)
  Environment: %s
  Changed files (first 5): %s

CORRELATION SIGNALS
%s`,
		bc.Service,
		bc.PeriodStart.Format("Jan 2 15:04 UTC"),
		bc.PeriodEnd.Format("Jan 2 15:04 UTC"),
		bc.AmountUSD, bc.PrevAmountUSD, bc.DeltaPct,
		bc.AnomalyScore,
		formatTags(bc.Tags),
		bc.PRNumber, bc.PRTitle,
		bc.PRAuthor, bc.PRTeam,
		bc.DeployTime.Format("Jan 2 15:04 UTC"),
		deployTiming(bc.PeriodStart, bc.DeployTime),
		bc.Environment,
		formatFiles(bc.ChangedFiles, 5),
		formatFactors(bc.ConfidenceFactors),
	)

	return system, user
}

func formatTags(tags map[string]string) string {
	if len(tags) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(tags))
	for k, v := range tags {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ", ")
}

func formatHours(h float64) string {
	if h < 1 {
		return fmt.Sprintf("%.0f minutes", h*60)
	}
	return fmt.Sprintf("%.1f hours", h)
}

// deployTiming describes when a deploy happened relative to the cost period.
// Deploys made during the period are common (a DAILY period is a whole UTC
// day), so a negative offset reads "into the period" rather than "-3 hours".
func deployTiming(periodStart, deployTime time.Time) string {
	h := periodStart.Sub(deployTime).Hours()
	if h < 0 {
		return formatHours(-h) + " into the cost period"
	}
	return formatHours(h) + " before the cost period began"
}

func formatFiles(files []string, max int) string {
	if len(files) == 0 {
		return "(none)"
	}
	if len(files) > max {
		files = files[:max]
	}
	return strings.Join(files, ", ")
}

func formatFactors(factors []models.ConfidenceFactor) string {
	var sb strings.Builder
	for _, f := range factors {
		sb.WriteString(fmt.Sprintf("  %-22s score=%.2f  %s\n", f.Name, f.Score, f.Reason))
	}
	return sb.String()
}
