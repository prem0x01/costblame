package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Granularity controls the resolution of cost data fetched from billing APIs.
type Granularity string

const (
	GranularityDaily  Granularity = "DAILY"
	GranularityHourly Granularity = "HOURLY"
)

// DeploySource identifies which CI/CD system emitted a deployment event.
type DeploySource string

const (
	DeploySourceGitHubActions DeploySource = "github_actions"
	DeploySourceGitLabCI      DeploySource = "gitlab_ci"
	DeploySourceArgoCD        DeploySource = "argocd"
)

// DeployStatus reflects the final state of a deployment pipeline run.
type DeployStatus string

const (
	DeployStatusSuccess     DeployStatus = "success"
	DeployStatusFailure     DeployStatus = "failure"
	DeployStatusRolledBack  DeployStatus = "rolled_back"
)

// BlameStatus tracks the lifecycle of a blame edge from creation to resolution.
type BlameStatus string

const (
	BlameStatusPending   BlameStatus = "pending"   // scored, narrative not yet generated
	BlameStatusResolved  BlameStatus = "resolved"  // narrative generated, alert sent
	BlameStatusConfirmed BlameStatus = "confirmed" // user confirmed the blame
	BlameStatusDismissed BlameStatus = "dismissed" // user dismissed as false positive
)

// CostSnapshot is a point-in-time cost reading for one service/tag combination.
// It captures both the raw amount and its delta vs. the previous equivalent period,
// along with an anomaly score expressed as standard deviations from the rolling baseline.
type CostSnapshot struct {
	ID            uuid.UUID         `db:"id"             json:"id"`
	CollectedAt   time.Time         `db:"collected_at"   json:"collected_at"`
	PeriodStart   time.Time         `db:"period_start"   json:"period_start"`
	PeriodEnd     time.Time         `db:"period_end"     json:"period_end"`
	Source        string            `db:"source"         json:"source"`        // "aws", "gcp"
	Service       string            `db:"service"        json:"service"`       // "AmazonECS"
	Region        string            `db:"region"         json:"region"`
	Tags          map[string]string `db:"tags"           json:"tags"`
	AmountUSD     float64           `db:"amount_usd"     json:"amount_usd"`
	PrevAmountUSD float64           `db:"prev_amount_usd" json:"prev_amount_usd"`
	DeltaUSD      float64           `db:"delta_usd"      json:"delta_usd"`
	DeltaPct      float64           `db:"delta_pct"      json:"delta_pct"`
	IsAnomaly     bool              `db:"is_anomaly"     json:"is_anomaly"`
	AnomalyScore  float64           `db:"anomaly_score"  json:"anomaly_score"` // stddev from 30d baseline
	Granularity   Granularity       `db:"granularity"    json:"granularity"`
}

// DeployEvent is a deployment that occurred, ingested from a CI/CD system.
// ChangedFiles and InferredServices are populated asynchronously after the initial
// webhook ack — the correlation engine re-reads enriched records before scoring.
type DeployEvent struct {
	ID               uuid.UUID       `db:"id"                json:"id"`
	OccurredAt       time.Time       `db:"occurred_at"       json:"occurred_at"`
	Source           DeploySource    `db:"source"            json:"source"`
	Repository       string          `db:"repository"        json:"repository"`
	Branch           string          `db:"branch"            json:"branch"`
	CommitSHA        string          `db:"commit_sha"        json:"commit_sha"`
	PRNumber         int             `db:"pr_number"         json:"pr_number"`
	PRTitle          string          `db:"pr_title"          json:"pr_title"`
	PRAuthor         string          `db:"pr_author"         json:"pr_author"`
	PRTeam           string          `db:"pr_team"           json:"pr_team"`
	PRLabels         []string        `db:"pr_labels"         json:"pr_labels"`
	ChangedFiles     []string        `db:"changed_files"     json:"changed_files"`
	InferredServices []string        `db:"inferred_services" json:"inferred_services"`
	Environment      string          `db:"environment"       json:"environment"` // "prod", "staging"
	Status           DeployStatus    `db:"status"            json:"status"`
	RawPayload       json.RawMessage `db:"raw_payload"       json:"raw_payload"`
}

// ConfidenceFactor is one scored signal that contributes to the overall blame score.
// Storing individual factors lets users understand why a blame was assigned.
type ConfidenceFactor struct {
	Name   string  `json:"name"`
	Score  float64 `json:"score"`  // 0.0–1.0 for this factor alone
	Weight float64 `json:"weight"` // its contribution to the final score
	Reason string  `json:"reason"` // human-readable explanation
}

// BlameEdge is a scored, optionally narrated correlation between one cost anomaly
// and one deployment event.
type BlameEdge struct {
	ID                uuid.UUID          `db:"id"                  json:"id"`
	CostSnapshotID    uuid.UUID          `db:"cost_snapshot_id"    json:"cost_snapshot_id"`
	DeployEventID     uuid.UUID          `db:"deploy_event_id"     json:"deploy_event_id"`
	ConfidenceScore   float64            `db:"confidence_score"    json:"confidence_score"`
	ConfidenceFactors []ConfidenceFactor `db:"confidence_factors"  json:"confidence_factors"`
	Narrative         string             `db:"narrative"           json:"narrative"`
	Status            BlameStatus        `db:"status"              json:"status"`
	CreatedAt         time.Time          `db:"created_at"          json:"created_at"`
	// Denormalized joins — populated by store on read, not persisted in columns.
	CostSnapshot *CostSnapshot `db:"-" json:"cost_snapshot,omitempty"`
	DeployEvent  *DeployEvent  `db:"-" json:"deploy_event,omitempty"`
}

// BlameGraph is the complete causal chain for a single cost anomaly.
// Edges are sorted descending by ConfidenceScore; TopBlame is edges[0].
type BlameGraph struct {
	ID          uuid.UUID   `json:"id"`
	Anomaly     CostSnapshot `json:"anomaly"`
	Edges       []BlameEdge  `json:"edges"`
	TopBlame    *BlameEdge   `json:"top_blame,omitempty"`
	GeneratedAt time.Time    `json:"generated_at"`
	AlertSent   bool         `json:"alert_sent"`
}
