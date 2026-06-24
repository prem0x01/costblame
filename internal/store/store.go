// Package store defines the persistence interface for costblame.
// The interface is implemented by both SQLite (for local/single-binary use)
// and PostgreSQL (for multi-instance deployments).
package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/prem0x01/costblame/pkg/models"
)

// Store is the unified persistence interface for all costblame data.
type Store interface {
	// --- CostSnapshot ---

	// SaveCostSnapshot persists a new cost snapshot. Duplicate IDs are ignored.
	SaveCostSnapshot(ctx context.Context, s models.CostSnapshot) error

	// SaveCostSnapshots bulk-inserts cost snapshots. Duplicate IDs are ignored.
	SaveCostSnapshots(ctx context.Context, ss []models.CostSnapshot) error

	// UnblamedAnomalies returns anomalous cost snapshots that have no BlameEdge yet.
	UnblamedAnomalies(ctx context.Context) ([]models.CostSnapshot, error)

	// CostSnapshotsByService returns snapshots for a service, sorted ascending by PeriodStart.
	CostSnapshotsByService(ctx context.Context, service string, from, to time.Time) ([]models.CostSnapshot, error)

	// --- DeployEvent ---

	// UpsertDeployEvent inserts or updates a deploy event by ID.
	// This supports the two-phase enrichment pattern: a partial event is
	// inserted on webhook receipt, then updated with PR/file data asynchronously.
	UpsertDeployEvent(ctx context.Context, e models.DeployEvent) error

	// DeploysBetween returns deploy events whose OccurredAt falls in [from, to].
	DeploysBetween(ctx context.Context, from, to time.Time) ([]models.DeployEvent, error)

	// --- BlameEdge ---

	// SaveBlameEdges bulk-inserts blame edges. Duplicate IDs are ignored.
	SaveBlameEdges(ctx context.Context, edges []models.BlameEdge) error

	// UpdateBlameEdge updates the narrative, status, and confidence score of an existing edge.
	UpdateBlameEdge(ctx context.Context, edge models.BlameEdge) error

	// BlameEdgesBySnapshot returns all edges for a cost snapshot, sorted by ConfidenceScore desc.
	BlameEdgesBySnapshot(ctx context.Context, snapshotID uuid.UUID) ([]models.BlameEdge, error)

	// RecentBlameEdges returns the most recent resolved blame edges, up to limit.
	RecentBlameEdges(ctx context.Context, limit int) ([]models.BlameEdge, error)

	// ConfirmedBlameCount returns how many confirmed blame edges exist for the
	// given (author, service) pair — used by the historical scoring factor.
	ConfirmedBlameCount(ctx context.Context, author, service string) (int, error)

	// --- Lifecycle ---

	// Migrate runs all pending schema migrations.
	Migrate(ctx context.Context) error

	// Close releases database resources.
	Close() error
}
