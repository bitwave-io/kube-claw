// Package sqlite is the v0 default store.Store implementation.
//
// It is single-writer and tuned with WAL + busy_timeout. Those knobs live HERE,
// never in the store.Store interface, so the interface stays portable to
// Postgres/Spanner (DESIGN.md §7).
package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)

	"github.com/traego/kube-claw/internal/store"
)

// Store is the SQLite-backed store.Store.
type Store struct {
	db *sql.DB
}

var _ store.Store = (*Store)(nil)

// Open opens (creating if absent) the SQLite database at path and applies
// connection pragmas. Call Migrate before use.
//
// WAL gives concurrent readers alongside the single writer; busy_timeout avoids
// spurious SQLITE_BUSY under the reconciler + API + router touching the DB.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single writer: cap the pool to one connection.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Tx runs fn inside a transaction, rolling back on error.
func (s *Store) Tx(ctx context.Context, fn func(store.Tx) error) error {
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if err := fn(&tx{tx: sqlTx}); err != nil {
		_ = sqlTx.Rollback()
		return err
	}
	return sqlTx.Commit()
}

// tx implements store.Tx over a *sql.Tx.
type tx struct {
	tx *sql.Tx
}

// AppendAudit writes a hash-chained audit row: row_hash = sha256(prev_hash | fields).
// A consumer can verify the chain by recomputing each row_hash in id order — any
// edited or removed row breaks the chain (tamper-evident, DESIGN.md §21).
func (t *tx) AppendAudit(ev store.AuditEvent) error {
	var prev sql.NullString
	// Last row_hash within this tx (NULL/empty for the first row).
	_ = t.tx.QueryRow(`SELECT row_hash FROM audit ORDER BY id DESC LIMIT 1`).Scan(&prev)

	detail, err := json.Marshal(ev.Detail)
	if err != nil {
		return fmt.Errorf("marshal audit detail: %w", err)
	}
	ts := store.NowRFC3339()
	payload := strings.Join([]string{
		prev.String, ts, ev.Type, ev.RunID, ev.GrantID, ev.SecretID, ev.Actor, string(detail),
	}, "|")
	sum := sha256.Sum256([]byte(payload))
	rowHash := hex.EncodeToString(sum[:])

	_, err = t.tx.Exec(
		`INSERT INTO audit (ts,type,run_id,grant_id,secret_id,actor,detail,prev_hash,row_hash)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		ts, ev.Type, ev.RunID, ev.GrantID, ev.SecretID, ev.Actor, string(detail), prev.String, rowHash,
	)
	if err != nil {
		return fmt.Errorf("append audit: %w", err)
	}
	return nil
}

// CreateRun inserts a new run row.
func (t *tx) CreateRun(r store.Run) error {
	if r.CreatedAt == "" {
		r.CreatedAt = store.NowRFC3339()
	}
	_, err := t.tx.Exec(
		`INSERT INTO runs (id,agent_ns,agent_name,session_id,phase,source,input,created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		r.ID, r.AgentNamespace, r.AgentName, r.SessionID, r.Phase, r.Source, r.Input, r.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create run: %w", err)
	}
	return nil
}

func scanRun(s interface{ Scan(...any) error }) (store.Run, error) {
	var r store.Run
	var session, source, input, assigned, podUID, started, completed sql.NullString
	err := s.Scan(&r.ID, &r.AgentNamespace, &r.AgentName, &session, &r.Phase,
		&source, &input, &assigned, &podUID, &r.CreatedAt, &started, &completed)
	if err != nil {
		return r, err
	}
	r.SessionID, r.Source, r.Input = session.String, source.String, input.String
	r.AssignedPod, r.PodUID = assigned.String, podUID.String
	r.StartedAt, r.CompletedAt = started.String, completed.String
	return r, nil
}

const runCols = `id,agent_ns,agent_name,session_id,phase,source,input,assigned_pod,pod_uid,created_at,started_at,completed_at`

// GetRun returns a run by id, or store.ErrNotFound.
func (t *tx) GetRun(id string) (store.Run, error) {
	row := t.tx.QueryRow(`SELECT `+runCols+` FROM runs WHERE id=?`, id)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Run{}, store.ErrNotFound
	}
	return r, err
}

// ListRuns returns the most recent runs, newest first.
func (t *tx) ListRuns(limit int) ([]store.Run, error) {
	return t.queryRuns(`SELECT `+runCols+` FROM runs ORDER BY created_at DESC LIMIT ?`, limit)
}

// ListRunsByPhase returns runs in a phase, oldest first (FIFO processing order).
func (t *tx) ListRunsByPhase(phase string, limit int) ([]store.Run, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := t.tx.Query(`SELECT `+runCols+` FROM runs WHERE phase=? ORDER BY created_at ASC LIMIT ?`, phase, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectRuns(rows)
}

func (t *tx) queryRuns(q string, args ...any) ([]store.Run, error) {
	rows, err := t.tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectRuns(rows)
}

func collectRuns(rows *sql.Rows) ([]store.Run, error) {
	var out []store.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkRunRunning sets phase=Running, the assigned pod, and started_at.
func (t *tx) MarkRunRunning(id, pod string) error {
	_, err := t.tx.Exec(
		`UPDATE runs SET phase='Running', assigned_pod=?, started_at=? WHERE id=?`,
		pod, store.NowRFC3339(), id)
	return err
}

// SeenEvent records a connector event and reports whether it was a duplicate.
func (t *tx) SeenEvent(source, eventID string) (bool, error) {
	res, err := t.tx.Exec(
		`INSERT OR IGNORE INTO dedupe (source, event_id, seen_at) VALUES (?,?,?)`,
		source, eventID, store.NowRFC3339())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 0, nil // 0 rows inserted == already present == duplicate
}

// MarkRunBlocked sets phase=Blocked (awaiting secret approval).
func (t *tx) MarkRunBlocked(id string) error {
	_, err := t.tx.Exec(`UPDATE runs SET phase='Blocked' WHERE id=?`, id)
	return err
}

// MarkRunSucceeded sets phase=Succeeded and completed_at.
func (t *tx) MarkRunSucceeded(id string) error {
	_, err := t.tx.Exec(
		`UPDATE runs SET phase='Succeeded', completed_at=? WHERE id=?`,
		store.NowRFC3339(), id)
	return err
}

// MarkRunFailed sets phase=Failed and completed_at.
func (t *tx) MarkRunFailed(id string) error {
	_, err := t.tx.Exec(
		`UPDATE runs SET phase='Failed', completed_at=? WHERE id=?`,
		store.NowRFC3339(), id)
	return err
}

// AppendOutput records an output produced by a run.
func (t *tx) AppendOutput(runID string, out store.Output) error {
	ts := out.CreatedAt
	if ts == "" {
		ts = store.NowRFC3339()
	}
	_, err := t.tx.Exec(
		`INSERT INTO run_outputs (run_id, kind, content, created_at) VALUES (?,?,?,?)`,
		runID, out.Kind, out.Content, ts)
	return err
}

// ListOutputs returns a run's outputs, oldest first.
func (t *tx) ListOutputs(runID string) ([]store.Output, error) {
	rows, err := t.tx.Query(
		`SELECT kind, content, created_at FROM run_outputs WHERE run_id=? ORDER BY id ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Output
	for rows.Next() {
		var o store.Output
		if err := rows.Scan(&o.Kind, &o.Content, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
