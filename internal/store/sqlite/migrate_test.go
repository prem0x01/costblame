package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Upgrade path: a database from before migration 003 holds duplicate edges and
// no alert bookkeeping. Migrating must merge the duplicates (keeping the one a
// human reviewed), backfill alerted_at so old alerts are not re-sent, and then
// refuse new duplicates.
func TestMigration003_MergesDuplicatesAndBackfillsAlerts(t *testing.T) {
	ctx := context.Background()
	s, err := New(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Bring the schema to its pre-003 state, as an existing install would be.
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at DATETIME NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"001_initial.sql", "002_anomaly_scored_at.sql"} {
		body, err := migrationsFS.ReadFile("migrations/" + v)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("applying %s: %v", v, err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, v, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}

	t0 := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO cost_snapshots (id, collected_at, period_start, period_end, source, service) VALUES ('s1', ?, ?, ?, 'aws', 'AWSLambda')`, t0, t0, t0.Add(24*time.Hour))
	for _, d := range []string{"d1", "d2", "d3", "d4"} {
		exec(`INSERT INTO deploy_events (id, occurred_at, source) VALUES (?, ?, 'github_actions')`, d, t0)
	}
	edge := func(id, deploy, status string, age time.Duration) {
		exec(`INSERT INTO blame_edges (id, cost_snapshot_id, deploy_event_id, confidence_score, status, created_at) VALUES (?, 's1', ?, 0.8, ?, ?)`,
			id, deploy, status, t0.Add(age))
	}
	// d1: three duplicates; the confirmed one must win.
	edge("d1-pending", "d1", "pending", 0)
	edge("d1-resolved", "d1", "resolved", time.Minute)
	edge("d1-confirmed", "d1", "confirmed", 2*time.Minute)
	// d2: a lone pending edge: keeps alerted_at NULL.
	edge("d2-pending", "d2", "pending", 0)
	// d3: a lone resolved edge: was alerted by the old code, so it is backfilled.
	edge("d3-resolved", "d3", "resolved", 0)
	// d4: two pending duplicates; the older wins.
	edge("d4-new", "d4", "pending", time.Hour)
	edge("d4-old", "d4", "pending", 0)

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	ids := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM blame_edges`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[id] = true
	}
	rows.Close()

	for _, want := range []string{"d1-confirmed", "d2-pending", "d3-resolved", "d4-old"} {
		if !ids[want] {
			t.Errorf("edge %s should have survived the merge; have %v", want, ids)
		}
	}
	if len(ids) != 4 {
		t.Errorf("edges after merge = %d (%v), want one per pair = 4", len(ids), ids)
	}

	notAlerted := func(id string) bool {
		var isNull bool
		if err := s.db.QueryRowContext(ctx, `SELECT alerted_at IS NULL FROM blame_edges WHERE id = ?`, id).Scan(&isNull); err != nil {
			t.Fatal(err)
		}
		return isNull
	}
	for _, id := range []string{"d1-confirmed", "d3-resolved"} {
		if notAlerted(id) {
			t.Errorf("%s was already handled before the upgrade; alerted_at must be backfilled or it would be re-sent", id)
		}
	}
	if !notAlerted("d2-pending") {
		t.Error("a pending edge was never alerted; alerted_at should stay NULL")
	}

	// The unique index now blocks duplicates, whatever the ID.
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO blame_edges (id, cost_snapshot_id, deploy_event_id, confidence_score, status, created_at) VALUES ('dup', 's1', 'd2', 0.1, 'pending', ?)`, t0)
	if err == nil {
		t.Error("inserting a second edge for the same (anomaly, deploy) pair should violate the unique index")
	}

	// Re-running Migrate on an up-to-date database is a no-op.
	if err := s.Migrate(ctx); err != nil {
		t.Errorf("second Migrate: %v", err)
	}
}

// A migration that fails halfway must leave the schema exactly as it was and
// must not be recorded, so it can be retried after the cause is fixed.
func TestApplyMigration_RollsBackOnFailure(t *testing.T) {
	ctx := context.Background()
	s, err := New(filepath.Join(t.TempDir(), "rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	err = s.applyMigration(ctx, "999_broken.sql", `CREATE TABLE half_done (x INTEGER); CREATE TABLE half_done (x INTEGER);`)
	if err == nil {
		t.Fatal("expected the broken migration to fail")
	}

	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name = 'half_done'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("a failed migration left its first statement behind (tables=%d, err=%v)", n, err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version = '999_broken.sql'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("a failed migration was recorded as applied (rows=%d, err=%v)", n, err)
	}
}
