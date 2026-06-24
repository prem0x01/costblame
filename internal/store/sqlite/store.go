// Package sqlite implements the store.Store interface using SQLite via
// the mattn/go-sqlite3 driver. It embeds the migration SQL files so the
// binary is fully self-contained.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/google/uuid"

	"github.com/prem0x01/costblame/pkg/models"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store is the SQLite-backed implementation of store.Store.
type Store struct {
	db *sql.DB
}

// New opens a SQLite database at the given DSN and applies pending migrations.
func New(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite3", dsn+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite WAL supports one writer
	s := &Store{db: db}
	return s, nil
}

func (s *Store) Migrate(ctx context.Context) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}

	// Ensure the migrations tracking table exists before anything else.
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at DATETIME NOT NULL
		)`); err != nil {
		return fmt.Errorf("creating migrations table: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		version := entry.Name()

		var existing string
		err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_migrations WHERE version = ?`, version).Scan(&existing)
		if err == nil {
			continue // already applied
		}

		sql, err := migrationsFS.ReadFile("migrations/" + version)
		if err != nil {
			return err
		}

		if _, err := s.db.ExecContext(ctx, string(sql)); err != nil {
			return fmt.Errorf("applying migration %s: %w", version, err)
		}

		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, time.Now().UTC()); err != nil {
			return fmt.Errorf("recording migration %s: %w", version, err)
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// --- CostSnapshot ---

func (s *Store) SaveCostSnapshot(ctx context.Context, snap models.CostSnapshot) error {
	return s.SaveCostSnapshots(ctx, []models.CostSnapshot{snap})
}

func (s *Store) SaveCostSnapshots(ctx context.Context, snaps []models.CostSnapshot) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO cost_snapshots
			(id, collected_at, period_start, period_end, source, service, region,
			 tags, amount_usd, prev_amount_usd, delta_usd, delta_pct,
			 is_anomaly, anomaly_score, granularity)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, snap := range snaps {
		tags, err := json.Marshal(snap.Tags)
		if err != nil {
			return err
		}
		_, err = stmt.ExecContext(ctx,
			snap.ID.String(), snap.CollectedAt, snap.PeriodStart, snap.PeriodEnd,
			snap.Source, snap.Service, snap.Region, string(tags),
			snap.AmountUSD, snap.PrevAmountUSD, snap.DeltaUSD, snap.DeltaPct,
			boolToInt(snap.IsAnomaly), snap.AnomalyScore, string(snap.Granularity),
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UnblamedAnomalies(ctx context.Context) ([]models.CostSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT cs.id, cs.collected_at, cs.period_start, cs.period_end,
		       cs.source, cs.service, cs.region, cs.tags,
		       cs.amount_usd, cs.prev_amount_usd, cs.delta_usd, cs.delta_pct,
		       cs.is_anomaly, cs.anomaly_score, cs.granularity
		FROM cost_snapshots cs
		LEFT JOIN blame_edges be ON be.cost_snapshot_id = cs.id
		WHERE cs.is_anomaly = 1 AND be.id IS NULL
		ORDER BY cs.period_start DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSnapshots(rows)
}

func (s *Store) CostSnapshotsByService(ctx context.Context, service string, from, to time.Time) ([]models.CostSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, collected_at, period_start, period_end,
		       source, service, region, tags,
		       amount_usd, prev_amount_usd, delta_usd, delta_pct,
		       is_anomaly, anomaly_score, granularity
		FROM cost_snapshots
		WHERE service = ? AND period_start >= ? AND period_start < ?
		ORDER BY period_start ASC`, service, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSnapshots(rows)
}

// --- DeployEvent ---

func (s *Store) UpsertDeployEvent(ctx context.Context, e models.DeployEvent) error {
	labels, _ := json.Marshal(e.PRLabels)
	files, _ := json.Marshal(e.ChangedFiles)
	services, _ := json.Marshal(e.InferredServices)
	raw := e.RawPayload
	if raw == nil {
		raw = []byte("{}")
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO deploy_events
			(id, occurred_at, source, repository, branch, commit_sha,
			 pr_number, pr_title, pr_author, pr_team, pr_labels,
			 changed_files, inferred_services, environment, status, raw_payload)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			pr_number         = excluded.pr_number,
			pr_title          = excluded.pr_title,
			pr_author         = excluded.pr_author,
			pr_team           = excluded.pr_team,
			pr_labels         = excluded.pr_labels,
			changed_files     = excluded.changed_files,
			inferred_services = excluded.inferred_services`,
		e.ID.String(), e.OccurredAt, string(e.Source), e.Repository, e.Branch, e.CommitSHA,
		e.PRNumber, e.PRTitle, e.PRAuthor, e.PRTeam, string(labels),
		string(files), string(services), e.Environment, string(e.Status), string(raw),
	)
	return err
}

func (s *Store) DeploysBetween(ctx context.Context, from, to time.Time) ([]models.DeployEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, occurred_at, source, repository, branch, commit_sha,
		       pr_number, pr_title, pr_author, pr_team, pr_labels,
		       changed_files, inferred_services, environment, status, raw_payload
		FROM deploy_events
		WHERE occurred_at >= ? AND occurred_at <= ?
		ORDER BY occurred_at DESC`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDeployEvents(rows)
}

// --- BlameEdge ---

func (s *Store) SaveBlameEdges(ctx context.Context, edges []models.BlameEdge) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO blame_edges
			(id, cost_snapshot_id, deploy_event_id, confidence_score,
			 confidence_factors, narrative, status, created_at)
		VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range edges {
		factors, _ := json.Marshal(e.ConfidenceFactors)
		_, err = stmt.ExecContext(ctx,
			e.ID.String(), e.CostSnapshotID.String(), e.DeployEventID.String(),
			e.ConfidenceScore, string(factors), e.Narrative, string(e.Status), e.CreatedAt,
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UpdateBlameEdge(ctx context.Context, edge models.BlameEdge) error {
	factors, _ := json.Marshal(edge.ConfidenceFactors)
	_, err := s.db.ExecContext(ctx, `
		UPDATE blame_edges
		SET narrative = ?, status = ?, confidence_score = ?, confidence_factors = ?
		WHERE id = ?`,
		edge.Narrative, string(edge.Status), edge.ConfidenceScore, string(factors), edge.ID.String(),
	)
	return err
}

func (s *Store) BlameEdgesBySnapshot(ctx context.Context, snapshotID uuid.UUID) ([]models.BlameEdge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, cost_snapshot_id, deploy_event_id, confidence_score,
		       confidence_factors, narrative, status, created_at
		FROM blame_edges
		WHERE cost_snapshot_id = ?
		ORDER BY confidence_score DESC`, snapshotID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBlameEdges(rows)
}

func (s *Store) RecentBlameEdges(ctx context.Context, limit int) ([]models.BlameEdge, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, cost_snapshot_id, deploy_event_id, confidence_score,
		       confidence_factors, narrative, status, created_at
		FROM blame_edges
		WHERE status IN ('resolved','confirmed')
		ORDER BY created_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBlameEdges(rows)
}

func (s *Store) ConfirmedBlameCount(ctx context.Context, author, service string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM blame_edges be
		JOIN deploy_events de ON de.id = be.deploy_event_id
		JOIN cost_snapshots cs ON cs.id = be.cost_snapshot_id
		WHERE de.pr_author = ? AND cs.service = ? AND be.status = 'confirmed'`,
		author, service,
	).Scan(&count)
	return count, err
}

// --- scan helpers ---

func scanSnapshots(rows *sql.Rows) ([]models.CostSnapshot, error) {
	var snaps []models.CostSnapshot
	for rows.Next() {
		var s models.CostSnapshot
		var id, gran, tagsJSON string
		var isAnomaly int
		if err := rows.Scan(&id, &s.CollectedAt, &s.PeriodStart, &s.PeriodEnd,
			&s.Source, &s.Service, &s.Region, &tagsJSON,
			&s.AmountUSD, &s.PrevAmountUSD, &s.DeltaUSD, &s.DeltaPct,
			&isAnomaly, &s.AnomalyScore, &gran); err != nil {
			return nil, err
		}
		s.ID, _ = uuid.Parse(id)
		s.IsAnomaly = isAnomaly == 1
		s.Granularity = models.Granularity(gran)
		_ = json.Unmarshal([]byte(tagsJSON), &s.Tags)
		snaps = append(snaps, s)
	}
	return snaps, rows.Err()
}

func scanDeployEvents(rows *sql.Rows) ([]models.DeployEvent, error) {
	var events []models.DeployEvent
	for rows.Next() {
		var e models.DeployEvent
		var id, src, labelsJSON, filesJSON, servicesJSON, status string
		var rawPayload []byte
		if err := rows.Scan(&id, &e.OccurredAt, &src, &e.Repository, &e.Branch, &e.CommitSHA,
			&e.PRNumber, &e.PRTitle, &e.PRAuthor, &e.PRTeam, &labelsJSON,
			&filesJSON, &servicesJSON, &e.Environment, &status, &rawPayload); err != nil {
			return nil, err
		}
		e.ID, _ = uuid.Parse(id)
		e.Source = models.DeploySource(src)
		e.Status = models.DeployStatus(status)
		e.RawPayload = rawPayload
		_ = json.Unmarshal([]byte(labelsJSON), &e.PRLabels)
		_ = json.Unmarshal([]byte(filesJSON), &e.ChangedFiles)
		_ = json.Unmarshal([]byte(servicesJSON), &e.InferredServices)
		events = append(events, e)
	}
	return events, rows.Err()
}

func scanBlameEdges(rows *sql.Rows) ([]models.BlameEdge, error) {
	var edges []models.BlameEdge
	for rows.Next() {
		var e models.BlameEdge
		var id, snapshotID, deployID, factorsJSON, status string
		if err := rows.Scan(&id, &snapshotID, &deployID, &e.ConfidenceScore,
			&factorsJSON, &e.Narrative, &status, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.ID, _ = uuid.Parse(id)
		e.CostSnapshotID, _ = uuid.Parse(snapshotID)
		e.DeployEventID, _ = uuid.Parse(deployID)
		e.Status = models.BlameStatus(status)
		_ = json.Unmarshal([]byte(factorsJSON), &e.ConfidenceFactors)
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
