// Package sqlite implements the store.Store interface using SQLite via
// the mattn/go-sqlite3 driver. It embeds the migration SQL files so the
// binary is fully self-contained.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"

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
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("checking migration %s: %w", version, err)
		}

		body, err := migrationsFS.ReadFile("migrations/" + version)
		if err != nil {
			return err
		}

		// Apply the migration and record it in one transaction, so a failure
		// leaves the schema exactly as it was and the migration can be retried.
		if err := s.applyMigration(ctx, version, string(body)); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration runs one migration file and records it atomically. SQLite DDL
// is transactional, so a failing statement rolls back the whole file. Migration
// files must not contain their own BEGIN/COMMIT.
func (s *Store) applyMigration(ctx context.Context, version, body string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("applying migration %s: %w", version, err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("applying migration %s: %w", version, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
		version, time.Now().UTC()); err != nil {
		return fmt.Errorf("recording migration %s: %w", version, err)
	}
	return tx.Commit()
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
		INSERT INTO cost_snapshots
			(id, collected_at, period_start, period_end, source, service, region,
			 tags, amount_usd, prev_amount_usd, delta_usd, delta_pct,
			 is_anomaly, anomaly_score, granularity)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			collected_at    = excluded.collected_at,
			tags            = excluded.tags,
			amount_usd      = excluded.amount_usd,
			prev_amount_usd = excluded.prev_amount_usd,
			delta_usd       = excluded.delta_usd,
			delta_pct       = excluded.delta_pct,
			is_anomaly      = excluded.is_anomaly,
			anomaly_score   = excluded.anomaly_score`)
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

func (s *Store) UnscoredAnomalies(ctx context.Context) ([]models.CostSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, collected_at, period_start, period_end,
		       source, service, region, tags,
		       amount_usd, prev_amount_usd, delta_usd, delta_pct,
		       is_anomaly, anomaly_score, granularity
		FROM cost_snapshots
		WHERE is_anomaly = 1 AND scored_at IS NULL
		ORDER BY period_start DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSnapshots(rows)
}

// RecentlyScoredAnomalies returns anomalies the engine first scored at or after
// since. The engine re-scores these so a deploy or enrichment that arrives after
// the first pass is still considered.
func (s *Store) RecentlyScoredAnomalies(ctx context.Context, since time.Time) ([]models.CostSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, collected_at, period_start, period_end,
		       source, service, region, tags,
		       amount_usd, prev_amount_usd, delta_usd, delta_pct,
		       is_anomaly, anomaly_score, granularity
		FROM cost_snapshots
		WHERE is_anomaly = 1 AND scored_at IS NOT NULL AND scored_at >= ?
		ORDER BY period_start DESC`, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSnapshots(rows)
}

func (s *Store) MarkAnomalyScored(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE cost_snapshots SET scored_at = ? WHERE id = ?`,
		time.Now().UTC(), id.String())
	return err
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

// UpsertDeployEvent inserts a deploy event, or merges it into the existing row
// with the same ID. Merging only ever fills blanks: a value already stored is
// never overwritten, so enrichment is additive and the order events arrive in
// (raw before enriched, or two workflows for one commit) does not matter. The
// one exception is occurred_at, which keeps the EARLIEST time — the first
// moment the code landed. Moving it forward would let a nightly scheduled run
// on an old commit make a days-old change look like a fresh deploy.
func (s *Store) UpsertDeployEvent(ctx context.Context, e models.DeployEvent) error {
	labels := jsonStrings(e.PRLabels)
	files := jsonStrings(e.ChangedFiles)
	services := jsonStrings(e.InferredServices)
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
			occurred_at       = MIN(deploy_events.occurred_at, excluded.occurred_at),
			repository        = COALESCE(NULLIF(deploy_events.repository, ''), excluded.repository),
			branch            = COALESCE(NULLIF(deploy_events.branch, ''), excluded.branch),
			commit_sha        = COALESCE(NULLIF(deploy_events.commit_sha, ''), excluded.commit_sha),
			pr_number         = CASE WHEN deploy_events.pr_number = 0 THEN excluded.pr_number ELSE deploy_events.pr_number END,
			pr_title          = COALESCE(NULLIF(deploy_events.pr_title, ''), excluded.pr_title),
			pr_author         = COALESCE(NULLIF(deploy_events.pr_author, ''), excluded.pr_author),
			pr_team           = COALESCE(NULLIF(deploy_events.pr_team, ''), excluded.pr_team),
			pr_labels         = CASE WHEN deploy_events.pr_labels IN ('', '[]', 'null') THEN excluded.pr_labels ELSE deploy_events.pr_labels END,
			changed_files     = CASE WHEN deploy_events.changed_files IN ('', '[]', 'null') THEN excluded.changed_files ELSE deploy_events.changed_files END,
			inferred_services = CASE WHEN deploy_events.inferred_services IN ('', '[]', 'null') THEN excluded.inferred_services ELSE deploy_events.inferred_services END,
			environment       = CASE WHEN deploy_events.environment IN ('', 'unknown') THEN excluded.environment ELSE deploy_events.environment END,
			raw_payload       = CASE WHEN deploy_events.raw_payload IN ('', '{}') THEN excluded.raw_payload ELSE deploy_events.raw_payload END`,
		// UTC so the text comparison behind MIN() and DeploysBetween() is
		// consistent no matter which offset a source reported.
		e.ID.String(), e.OccurredAt.UTC(), string(e.Source), e.Repository, e.Branch, e.CommitSHA,
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

// SaveBlameEdges writes edges in one transaction and is idempotent: an edge is
// identified by its (cost snapshot, deploy) pair. A pair that already exists is
// left alone if a human or the alert path has moved it past "pending"; a
// still-pending edge is refreshed with the new score, factors and (if it is
// being promoted) status and narrative. That lets a later scoring pass improve a
// stale candidate (say, after PR enrichment arrives) without ever overwriting a
// reviewed or already-alerted edge, or creating a duplicate.
func (s *Store) SaveBlameEdges(ctx context.Context, edges []models.BlameEdge) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO blame_edges
			(id, cost_snapshot_id, deploy_event_id, confidence_score,
			 confidence_factors, narrative, status, created_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(cost_snapshot_id, deploy_event_id) DO UPDATE SET
			confidence_score   = excluded.confidence_score,
			confidence_factors = excluded.confidence_factors,
			narrative          = CASE WHEN excluded.narrative != '' THEN excluded.narrative ELSE blame_edges.narrative END,
			status             = excluded.status
		WHERE blame_edges.status = 'pending'`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range edges {
		factors, err := json.Marshal(e.ConfidenceFactors)
		if err != nil {
			return err
		}
		_, err = stmt.ExecContext(ctx,
			e.ID.String(), e.CostSnapshotID.String(), e.DeployEventID.String(),
			e.ConfidenceScore, string(factors), e.Narrative, string(e.Status), e.CreatedAt.UTC(),
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UnalertedEdges returns resolved edges whose alert has not been delivered yet
// and that were created at or after since. It is the alert outbox: the engine
// delivers these and calls MarkEdgeAlerted on success, so a failed delivery is
// retried on the next cycle instead of being lost.
func (s *Store) UnalertedEdges(ctx context.Context, since time.Time) ([]models.BlameEdge, error) {
	rows, err := s.db.QueryContext(ctx, blameEdgeSelectJoined+`
		WHERE be.status = 'resolved' AND be.alerted_at IS NULL AND be.created_at >= ?
		ORDER BY be.created_at ASC`, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBlameEdgesWithJoins(rows)
}

// MarkEdgeAlerted records that an edge's alert was delivered.
func (s *Store) MarkEdgeAlerted(ctx context.Context, id uuid.UUID) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE blame_edges SET alerted_at = ? WHERE id = ?`, time.Now().UTC(), id.String())
	return err
}

func (s *Store) BlameEdgeByID(ctx context.Context, id uuid.UUID) (*models.BlameEdge, error) {
	rows, err := s.db.QueryContext(ctx, blameEdgeSelectJoined+`
		WHERE be.id = ?`, id.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	edges, err := scanBlameEdgesWithJoins(rows)
	if err != nil {
		return nil, err
	}
	if len(edges) == 0 {
		return nil, sql.ErrNoRows
	}
	return &edges[0], nil
}

func (s *Store) UpdateBlameStatus(ctx context.Context, id uuid.UUID, status models.BlameStatus) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE blame_edges SET status = ? WHERE id = ?`,
		string(status), id.String())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) BlameEdgesBySnapshot(ctx context.Context, snapshotID uuid.UUID) ([]models.BlameEdge, error) {
	rows, err := s.db.QueryContext(ctx, blameEdgeSelectJoined+`
		WHERE be.cost_snapshot_id = ?
		ORDER BY be.confidence_score DESC`, snapshotID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBlameEdgesWithJoins(rows)
}

func (s *Store) RecentBlameEdges(ctx context.Context, limit int) ([]models.BlameEdge, error) {
	rows, err := s.db.QueryContext(ctx, blameEdgeSelectJoined+`
		WHERE be.status IN ('resolved','confirmed')
		ORDER BY be.created_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBlameEdgesWithJoins(rows)
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

// blameEdgeSelectJoined selects a blame edge alongside its cost snapshot and
// deploy event rows, so callers get the denormalized BlameEdge.CostSnapshot /
// BlameEdge.DeployEvent fields populated on read (see models.BlameEdge).
// Callers append a WHERE/ORDER/LIMIT clause.
const blameEdgeSelectJoined = `
	SELECT be.id, be.cost_snapshot_id, be.deploy_event_id, be.confidence_score,
	       be.confidence_factors, be.narrative, be.status, be.created_at,
	       cs.collected_at, cs.period_start, cs.period_end, cs.source, cs.service,
	       cs.region, cs.tags, cs.amount_usd, cs.prev_amount_usd, cs.delta_usd,
	       cs.delta_pct, cs.is_anomaly, cs.anomaly_score, cs.granularity,
	       de.occurred_at, de.source, de.repository, de.branch, de.commit_sha,
	       de.pr_number, de.pr_title, de.pr_author, de.pr_team, de.pr_labels,
	       de.changed_files, de.inferred_services, de.environment, de.status, de.raw_payload
	FROM blame_edges be
	JOIN cost_snapshots cs ON cs.id = be.cost_snapshot_id
	JOIN deploy_events de ON de.id = be.deploy_event_id`

func scanBlameEdgesWithJoins(rows *sql.Rows) ([]models.BlameEdge, error) {
	var edges []models.BlameEdge
	for rows.Next() {
		var e models.BlameEdge
		var id, snapshotID, deployID, factorsJSON, status string

		var snap models.CostSnapshot
		var snapGran, snapTagsJSON string
		var snapIsAnomaly int

		var deploy models.DeployEvent
		var deploySource, deployLabelsJSON, deployFilesJSON, deployServicesJSON, deployStatus string
		var deployRaw []byte

		if err := rows.Scan(
			&id, &snapshotID, &deployID, &e.ConfidenceScore,
			&factorsJSON, &e.Narrative, &status, &e.CreatedAt,
			&snap.CollectedAt, &snap.PeriodStart, &snap.PeriodEnd, &snap.Source, &snap.Service,
			&snap.Region, &snapTagsJSON, &snap.AmountUSD, &snap.PrevAmountUSD, &snap.DeltaUSD,
			&snap.DeltaPct, &snapIsAnomaly, &snap.AnomalyScore, &snapGran,
			&deploy.OccurredAt, &deploySource, &deploy.Repository, &deploy.Branch, &deploy.CommitSHA,
			&deploy.PRNumber, &deploy.PRTitle, &deploy.PRAuthor, &deploy.PRTeam, &deployLabelsJSON,
			&deployFilesJSON, &deployServicesJSON, &deploy.Environment, &deployStatus, &deployRaw,
		); err != nil {
			return nil, err
		}

		e.ID, _ = uuid.Parse(id)
		e.CostSnapshotID, _ = uuid.Parse(snapshotID)
		e.DeployEventID, _ = uuid.Parse(deployID)
		e.Status = models.BlameStatus(status)
		_ = json.Unmarshal([]byte(factorsJSON), &e.ConfidenceFactors)

		snap.ID = e.CostSnapshotID
		snap.IsAnomaly = snapIsAnomaly == 1
		snap.Granularity = models.Granularity(snapGran)
		_ = json.Unmarshal([]byte(snapTagsJSON), &snap.Tags)
		e.CostSnapshot = &snap

		deploy.ID = e.DeployEventID
		deploy.Source = models.DeploySource(deploySource)
		deploy.Status = models.DeployStatus(deployStatus)
		deploy.RawPayload = deployRaw
		_ = json.Unmarshal([]byte(deployLabelsJSON), &deploy.PRLabels)
		_ = json.Unmarshal([]byte(deployFilesJSON), &deploy.ChangedFiles)
		_ = json.Unmarshal([]byte(deployServicesJSON), &deploy.InferredServices)
		e.DeployEvent = &deploy

		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// jsonStrings encodes a string slice as a JSON array, writing "[]" for nil
// (json.Marshal would produce "null", which the merge logic would then have to
// special-case).
func jsonStrings(v []string) string {
	if v == nil {
		return "[]"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
