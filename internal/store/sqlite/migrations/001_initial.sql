-- Initial schema for costblame.
-- SQLite does not support ALTER TABLE ADD CONSTRAINT, so foreign key enforcement
-- is enabled per-connection via PRAGMA foreign_keys = ON.

CREATE TABLE IF NOT EXISTS cost_snapshots (
    id              TEXT PRIMARY KEY,
    collected_at    DATETIME NOT NULL,
    period_start    DATETIME NOT NULL,
    period_end      DATETIME NOT NULL,
    source          TEXT NOT NULL,
    service         TEXT NOT NULL,
    region          TEXT NOT NULL DEFAULT '',
    tags            TEXT NOT NULL DEFAULT '{}',   -- JSON object
    amount_usd      REAL NOT NULL DEFAULT 0,
    prev_amount_usd REAL NOT NULL DEFAULT 0,
    delta_usd       REAL NOT NULL DEFAULT 0,
    delta_pct       REAL NOT NULL DEFAULT 0,
    is_anomaly      INTEGER NOT NULL DEFAULT 0,   -- 0/1 boolean
    anomaly_score   REAL NOT NULL DEFAULT 0,
    granularity     TEXT NOT NULL DEFAULT 'DAILY'
);

CREATE INDEX IF NOT EXISTS idx_cost_snapshots_period
    ON cost_snapshots (period_start, service);

CREATE INDEX IF NOT EXISTS idx_cost_snapshots_anomaly
    ON cost_snapshots (is_anomaly, period_start);

-- ----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS deploy_events (
    id                  TEXT PRIMARY KEY,
    occurred_at         DATETIME NOT NULL,
    source              TEXT NOT NULL,
    repository          TEXT NOT NULL DEFAULT '',
    branch              TEXT NOT NULL DEFAULT '',
    commit_sha          TEXT NOT NULL DEFAULT '',
    pr_number           INTEGER NOT NULL DEFAULT 0,
    pr_title            TEXT NOT NULL DEFAULT '',
    pr_author           TEXT NOT NULL DEFAULT '',
    pr_team             TEXT NOT NULL DEFAULT '',
    pr_labels           TEXT NOT NULL DEFAULT '[]',    -- JSON array
    changed_files       TEXT NOT NULL DEFAULT '[]',    -- JSON array
    inferred_services   TEXT NOT NULL DEFAULT '[]',    -- JSON array
    environment         TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL DEFAULT 'success',
    raw_payload         TEXT NOT NULL DEFAULT '{}'     -- JSON object
);

CREATE INDEX IF NOT EXISTS idx_deploy_events_occurred
    ON deploy_events (occurred_at);

CREATE INDEX IF NOT EXISTS idx_deploy_events_author
    ON deploy_events (pr_author);

-- ----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS blame_edges (
    id                  TEXT PRIMARY KEY,
    cost_snapshot_id    TEXT NOT NULL REFERENCES cost_snapshots(id),
    deploy_event_id     TEXT NOT NULL REFERENCES deploy_events(id),
    confidence_score    REAL NOT NULL DEFAULT 0,
    confidence_factors  TEXT NOT NULL DEFAULT '[]',   -- JSON array of ConfidenceFactor
    narrative           TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL DEFAULT 'pending',
    created_at          DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_blame_edges_snapshot
    ON blame_edges (cost_snapshot_id);

CREATE INDEX IF NOT EXISTS idx_blame_edges_status
    ON blame_edges (status, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_blame_edges_confidence
    ON blame_edges (confidence_score DESC);

-- ----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS schema_migrations (
    version TEXT PRIMARY KEY,
    applied_at DATETIME NOT NULL
);
