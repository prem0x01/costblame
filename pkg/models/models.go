package models

import (
	"encoding/json"
	"strings"
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
	DeployStatusSuccess    DeployStatus = "success"
	DeployStatusFailure    DeployStatus = "failure"
	DeployStatusRolledBack DeployStatus = "rolled_back"
)

// BlameStatus tracks the lifecycle of a blame edge from creation to resolution.
type BlameStatus string

const (
	BlameStatusPending   BlameStatus = "pending"   // scored, narrative not yet generated
	BlameStatusResolved  BlameStatus = "resolved"  // narrative generated, alert sent
	BlameStatusConfirmed BlameStatus = "confirmed" // user confirmed the blame
	BlameStatusDismissed BlameStatus = "dismissed" // user dismissed as false positive
)

// snapshotNamespace is the UUIDv5 namespace for deterministic snapshot IDs.
var snapshotNamespace = uuid.MustParse("b71e6e63-5f2c-4a4e-9d5a-3c1f0d9b8a01")

// SnapshotID derives a deterministic ID from a snapshot's natural key
// (source, service, period, granularity). Collecting the same period twice
// yields the same ID, so re-polls upsert instead of duplicating rows.
func SnapshotID(source, service string, periodStart, periodEnd time.Time, gran Granularity) uuid.UUID {
	key := source + "|" + service + "|" +
		periodStart.UTC().Format(time.RFC3339) + "|" +
		periodEnd.UTC().Format(time.RFC3339) + "|" +
		string(gran)
	return uuid.NewSHA1(snapshotNamespace, []byte(key))
}

// deployNamespace is the UUIDv5 namespace for deterministic deploy event IDs.
var deployNamespace = uuid.MustParse("5d0c2a61-7f1e-4b0a-8c3d-2e9b6f4a1c72")

// DeployEventID derives a deterministic ID from a deploy's natural key, e.g.
// (repository, commit SHA). Webhook redeliveries, several workflows finishing
// for one commit, and the two-phase enrichment all resolve to the same ID, so
// the store upserts one row per deploy instead of inserting duplicates that
// would each become a separate blame candidate.
//
// Trade-off: re-deploying the same commit (or an ArgoCD resync of the same
// revision) maps onto the original row. That is intended — the code did not
// change, so it is not a new candidate cause for a later cost spike.
func DeployEventID(source DeploySource, parts ...string) uuid.UUID {
	key := string(source) + "|" + strings.Join(parts, "|")
	return uuid.NewSHA1(deployNamespace, []byte(strings.ToLower(key)))
}

// CostSnapshot is a point-in-time cost reading for one service/tag combination.
// It captures both the raw amount and its delta vs. the previous equivalent period,
// along with an anomaly score expressed as standard deviations from the rolling baseline.
type CostSnapshot struct {
	ID            uuid.UUID         `db:"id"             json:"id"`
	CollectedAt   time.Time         `db:"collected_at"   json:"collected_at"`
	PeriodStart   time.Time         `db:"period_start"   json:"period_start"`
	PeriodEnd     time.Time         `db:"period_end"     json:"period_end"`
	Source        string            `db:"source"         json:"source"`  // "aws", "gcp"
	Service       string            `db:"service"        json:"service"` // "AmazonECS"
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
	ID               uuid.UUID    `db:"id"                json:"id"`
	OccurredAt       time.Time    `db:"occurred_at"       json:"occurred_at"`
	Source           DeploySource `db:"source"            json:"source"`
	Repository       string       `db:"repository"        json:"repository"`
	Branch           string       `db:"branch"            json:"branch"`
	CommitSHA        string       `db:"commit_sha"        json:"commit_sha"`
	PRNumber         int          `db:"pr_number"         json:"pr_number"`
	PRTitle          string       `db:"pr_title"          json:"pr_title"`
	PRAuthor         string       `db:"pr_author"         json:"pr_author"`
	PRTeam           string       `db:"pr_team"           json:"pr_team"`
	PRLabels         []string     `db:"pr_labels"         json:"pr_labels"`
	ChangedFiles     []string     `db:"changed_files"     json:"changed_files"`
	InferredServices []string     `db:"inferred_services" json:"inferred_services"`
	Environment      string       `db:"environment"       json:"environment"` // "prod", "staging"
	Status           DeployStatus `db:"status"            json:"status"`
	// RawPayload is persisted for debugging but never serialized: it can hold
	// full webhook bodies (emails, internal repo URLs) and the JSON API must
	// not hand those out.
	RawPayload json.RawMessage `db:"raw_payload" json:"-"`
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
	ID          uuid.UUID    `json:"id"`
	Anomaly     CostSnapshot `json:"anomaly"`
	Edges       []BlameEdge  `json:"edges"`
	TopBlame    *BlameEdge   `json:"top_blame,omitempty"`
	GeneratedAt time.Time    `json:"generated_at"`
	AlertSent   bool         `json:"alert_sent"`
}
