package web

// SystemStatus reports which real integrations are wired up for this running
// instance, so the UI can tell a new operator what's connected vs still to
// configure. It's built once at startup from the actual serve-time
// configuration (cmd/costblame/main.go) — never inferred or guessed.
type SystemStatus struct {
	// CostProvider is the configured billing adapter name (e.g. "aws"), or ""
	// if cost collection isn't running. Note this reflects configuration, not
	// verified credentials — a misconfigured provider still reports its name
	// here (the poller logs the real connection failure separately).
	CostProvider string

	// DeploySources lists every enabled deploy-event adapter (e.g.
	// "github", "gitlab", "argocd"). Empty means anomalies can be detected
	// but never attributed to a deploy.
	DeploySources []string

	// NarrativeEngine is "template" (no LLM configured) or the resolved LLM
	// provider name ("anthropic", "openai", "ollama").
	NarrativeEngine string
}

func (s SystemStatus) HasCostSource() bool   { return s.CostProvider != "" }
func (s SystemStatus) HasDeploySource() bool { return len(s.DeploySources) > 0 }
func (s SystemStatus) UsingLLM() bool        { return s.NarrativeEngine != "" && s.NarrativeEngine != "template" }

// Ready reports whether both halves of the correlation pipeline (a cost
// source and at least one deploy source) are configured.
func (s SystemStatus) Ready() bool { return s.HasCostSource() && s.HasDeploySource() }

// RequiredStepsDone/RequiredStepsTotal back the setup page's step-by-step
// progress indicator — only the cost source and deploy source are required,
// the narrative engine is optional.
func (s SystemStatus) RequiredStepsDone() int {
	n := 0
	if s.HasCostSource() {
		n++
	}
	if s.HasDeploySource() {
		n++
	}
	return n
}

func (s SystemStatus) RequiredStepsTotal() int { return 2 }

// ProgressPct is RequiredStepsDone/RequiredStepsTotal as a 0-100 integer,
// for the setup page's progress bar width.
func (s SystemStatus) ProgressPct() int {
	return s.RequiredStepsDone() * 100 / s.RequiredStepsTotal()
}
