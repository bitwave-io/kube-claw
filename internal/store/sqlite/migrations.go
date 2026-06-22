package sqlite

import (
	"context"
	"fmt"
)

// migrations is the ordered list of schema statements (DESIGN.md §7). All are
// idempotent (IF NOT EXISTS); the full schema is created up front, repository
// methods for each table land in their respective phases.
var migrations = []string{
	`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL);`,

	// Append-only, hash-chained audit log (tamper-evident).
	`CREATE TABLE IF NOT EXISTS audit (
		id        INTEGER PRIMARY KEY AUTOINCREMENT,
		ts        TEXT NOT NULL,
		type      TEXT NOT NULL,
		run_id    TEXT,
		grant_id  TEXT,
		secret_id TEXT,
		actor     TEXT,
		detail    TEXT,
		prev_hash TEXT,
		row_hash  TEXT NOT NULL
	);`,

	`CREATE TABLE IF NOT EXISTS secrets (
		id          TEXT PRIMARY KEY,
		namespace   TEXT NOT NULL,
		name        TEXT NOT NULL,
		type        TEXT,
		description TEXT,
		labels      TEXT,
		created_at  TEXT NOT NULL,
		UNIQUE(namespace, name)
	);`,
	`CREATE TABLE IF NOT EXISTS secret_granters (
		secret_id TEXT NOT NULL,
		principal TEXT NOT NULL
	);`,
	`CREATE TABLE IF NOT EXISTS secret_versions (
		id         TEXT PRIMARY KEY,
		secret_id  TEXT NOT NULL,
		ciphertext BLOB NOT NULL,
		checksum   TEXT,
		created_at TEXT NOT NULL,
		created_by TEXT
	);`,

	`CREATE TABLE IF NOT EXISTS grants (
		id              TEXT PRIMARY KEY,
		agent_ns        TEXT NOT NULL,
		agent_name      TEXT NOT NULL,
		service_account TEXT,
		image_digest    TEXT,
		agent_spec_hash TEXT,
		secret_id       TEXT,
		secret_version  TEXT,
		delivery_hash   TEXT,
		approved_by     TEXT,
		approved_at     TEXT,
		reason          TEXT,
		revoked_at      TEXT,
		revoked_reason  TEXT
	);`,
	`CREATE TABLE IF NOT EXISTS secret_requests (
		id           TEXT PRIMARY KEY,
		status       TEXT NOT NULL,
		agent_ns     TEXT,
		agent_name   TEXT,
		run_id       TEXT,
		secret_id    TEXT,
		secret_name  TEXT,
		image_digest TEXT,
		context      TEXT,
		created_at   TEXT NOT NULL,
		notified_at  TEXT
	);`,

	`CREATE TABLE IF NOT EXISTS runs (
		id           TEXT PRIMARY KEY,
		agent_ns     TEXT NOT NULL,
		agent_name   TEXT NOT NULL,
		session_id   TEXT,
		phase        TEXT NOT NULL,
		source       TEXT,
		input        TEXT,
		assigned_pod TEXT,
		pod_uid      TEXT,
		created_at   TEXT NOT NULL,
		started_at   TEXT,
		completed_at TEXT
	);`,
	`CREATE TABLE IF NOT EXISTS run_outputs (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id     TEXT NOT NULL,
		kind       TEXT NOT NULL,
		content    TEXT NOT NULL,
		created_at TEXT NOT NULL
	);`,
	`CREATE TABLE IF NOT EXISTS sessions (
		id         TEXT PRIMARY KEY,
		agent_ns   TEXT NOT NULL,
		agent_name TEXT NOT NULL,
		key        TEXT
	);`,
	`CREATE TABLE IF NOT EXISTS dedupe (
		source   TEXT NOT NULL,
		event_id TEXT NOT NULL,
		seen_at  TEXT NOT NULL,
		PRIMARY KEY (source, event_id)
	);`,
	`CREATE TABLE IF NOT EXISTS base_images (
		name        TEXT PRIMARY KEY,
		image       TEXT NOT NULL,
		description TEXT,
		created_at  TEXT NOT NULL
	);`,
	`CREATE TABLE IF NOT EXISTS intake_tokens (
		token_hash  TEXT PRIMARY KEY,
		secret_id   TEXT NOT NULL,
		created_at  TEXT NOT NULL,
		expires_at  TEXT NOT NULL,
		consumed_at TEXT
	);`,
	`CREATE TABLE IF NOT EXISTS prompts (
		agent_ns   TEXT NOT NULL,
		agent_name TEXT NOT NULL,
		content    TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (agent_ns, agent_name)
	);`,

	`CREATE INDEX IF NOT EXISTS grants_by_agent  ON grants(agent_ns, agent_name);`,
	`CREATE INDEX IF NOT EXISTS grants_by_secret ON grants(secret_id);`,
	`CREATE INDEX IF NOT EXISTS runs_by_agent    ON runs(agent_ns, agent_name);`,
	`CREATE INDEX IF NOT EXISTS runs_by_created   ON runs(created_at);`,
	`CREATE INDEX IF NOT EXISTS runs_by_phase     ON runs(phase);`,
	`CREATE INDEX IF NOT EXISTS run_outputs_by_run ON run_outputs(run_id);`,
	`CREATE INDEX IF NOT EXISTS audit_by_ts       ON audit(ts);`,
}

// Migrate applies pending migrations idempotently.
func (s *Store) Migrate(ctx context.Context) error {
	for i, stmt := range migrations {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration %d: %w", i, err)
		}
	}
	return nil
}
