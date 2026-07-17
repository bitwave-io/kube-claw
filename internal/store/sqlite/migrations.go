package sqlite

import (
	"context"
	"fmt"
	"strings"
)

// additiveColumns are columns added to existing tables after their initial
// CREATE. Run tolerantly so they apply to old DBs and no-op on new ones
// (CREATE TABLE already includes them) — SQLite has no ADD COLUMN IF NOT EXISTS.
var additiveColumns = []string{
	`ALTER TABLE secrets ADD COLUMN description TEXT`,
	`ALTER TABLE secret_requests ADD COLUMN notified_at TEXT`,
	`ALTER TABLE secret_requests ADD COLUMN requested_by TEXT`,
	`ALTER TABLE intake_tokens ADD COLUMN run_id TEXT`,
}

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
		requested_by TEXT,
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
		run_id      TEXT,
		created_at  TEXT NOT NULL,
		expires_at  TEXT NOT NULL,
		consumed_at TEXT
	);`,
	`CREATE TABLE IF NOT EXISTS schedules (
		id          TEXT PRIMARY KEY,
		agent_ns    TEXT NOT NULL,
		agent_name  TEXT NOT NULL,
		cron        TEXT NOT NULL,
		prompt      TEXT NOT NULL,
		channel     TEXT,
		enabled     INTEGER NOT NULL DEFAULT 1,
		last_run_at TEXT,
		next_run_at TEXT,
		created_at  TEXT NOT NULL
	);`,
	`CREATE TABLE IF NOT EXISTS prompts (
		agent_ns   TEXT NOT NULL,
		agent_name TEXT NOT NULL,
		content    TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (agent_ns, agent_name)
	);`,
	`CREATE TABLE IF NOT EXISTS connectors (
		id             TEXT PRIMARY KEY,
		name           TEXT NOT NULL UNIQUE,
		callback_url   TEXT NOT NULL,
		api_key_hash   TEXT NOT NULL UNIQUE,
		signing_secret TEXT NOT NULL,
		agent_ns       TEXT NOT NULL,
		agent_name     TEXT NOT NULL,
		disabled       INTEGER NOT NULL DEFAULT 0,
		created_at     TEXT NOT NULL
	);`,
	`CREATE TABLE IF NOT EXISTS git_repos (
		id               TEXT PRIMARY KEY,
		namespace        TEXT NOT NULL,
		name             TEXT NOT NULL,
		url              TEXT NOT NULL,
		description      TEXT,
		read_credential  TEXT,
		write_credential TEXT,
		created_at       TEXT NOT NULL,
		UNIQUE(namespace, name)
	);`,
	`CREATE TABLE IF NOT EXISTS git_repo_granters (
		repo_id   TEXT NOT NULL,
		principal TEXT NOT NULL
	);`,
	`CREATE TABLE IF NOT EXISTS git_repo_grants (
		id              TEXT PRIMARY KEY,
		agent_ns        TEXT NOT NULL,
		agent_name      TEXT NOT NULL,
		service_account TEXT,
		image_digest    TEXT,
		agent_spec_hash TEXT,
		repo_id         TEXT NOT NULL,
		access          TEXT NOT NULL,
		approved_by     TEXT,
		approved_at     TEXT,
		reason          TEXT,
		revoked_at      TEXT,
		revoked_reason  TEXT
	);`,
	`CREATE TABLE IF NOT EXISTS git_repo_requests (
		id           TEXT PRIMARY KEY,
		status       TEXT NOT NULL,
		agent_ns     TEXT,
		agent_name   TEXT,
		run_id       TEXT,
		repo_id      TEXT NOT NULL,
		repo_name    TEXT,
		access       TEXT NOT NULL,
		image_digest TEXT,
		context      TEXT,
		requested_by TEXT,
		created_at   TEXT NOT NULL,
		notified_at  TEXT
	);`,
	// Install-wide key/value settings (DESIGN.md §24.6, e.g. the upgrade admin).
	`CREATE TABLE IF NOT EXISTS settings (
		key        TEXT PRIMARY KEY,
		value      TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);`,
	`CREATE TABLE IF NOT EXISTS channel_configs (
		channel          TEXT PRIMARY KEY,
		agent_ns         TEXT NOT NULL,
		agent_name       TEXT NOT NULL,
		mention_required INTEGER NOT NULL DEFAULT 1,
		thread_only      INTEGER NOT NULL DEFAULT 1,
		updated_at       TEXT NOT NULL
	);`,

	`CREATE INDEX IF NOT EXISTS grants_by_agent  ON grants(agent_ns, agent_name);`,
	`CREATE INDEX IF NOT EXISTS grants_by_secret ON grants(secret_id);`,
	`CREATE INDEX IF NOT EXISTS git_repo_grants_by_agent ON git_repo_grants(agent_ns, agent_name);`,
	`CREATE INDEX IF NOT EXISTS git_repo_grants_by_repo  ON git_repo_grants(repo_id);`,
	`CREATE INDEX IF NOT EXISTS git_repo_granters_by_repo ON git_repo_granters(repo_id);`,
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
	// Additive columns: ignore "duplicate column" so they apply once to old DBs.
	for _, a := range additiveColumns {
		if _, err := s.db.ExecContext(ctx, a); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("alter: %w", err)
		}
	}
	return nil
}
