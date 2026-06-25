package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/traego/kube-claw/internal/store"
)

const schedCols = `id, agent_ns, agent_name, cron, prompt, channel, enabled, last_run_at, next_run_at, created_at`

// SetSchedule creates or replaces a schedule.
func (t *tx) SetSchedule(s store.Schedule) error {
	if s.CreatedAt == "" {
		s.CreatedAt = store.NowRFC3339()
	}
	enabled := 0
	if s.Enabled {
		enabled = 1
	}
	_, err := t.tx.Exec(
		`INSERT INTO schedules (`+schedCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   agent_ns=excluded.agent_ns, agent_name=excluded.agent_name, cron=excluded.cron,
		   prompt=excluded.prompt, channel=excluded.channel, enabled=excluded.enabled,
		   last_run_at=excluded.last_run_at, next_run_at=excluded.next_run_at`,
		s.ID, s.AgentNamespace, s.AgentName, s.Cron, s.Prompt, s.Channel, enabled,
		s.LastRunAt, s.NextRunAt, s.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("set schedule: %w", err)
	}
	return nil
}

func scanSchedule(s interface{ Scan(...any) error }) (store.Schedule, error) {
	var sc store.Schedule
	var enabled int
	var channel, lastRun, nextRun sql.NullString
	err := s.Scan(&sc.ID, &sc.AgentNamespace, &sc.AgentName, &sc.Cron, &sc.Prompt,
		&channel, &enabled, &lastRun, &nextRun, &sc.CreatedAt)
	if err != nil {
		return sc, err
	}
	sc.Channel, sc.LastRunAt, sc.NextRunAt = channel.String, lastRun.String, nextRun.String
	sc.Enabled = enabled != 0
	return sc, nil
}

// GetSchedule returns a schedule by id.
func (t *tx) GetSchedule(id string) (store.Schedule, error) {
	sc, err := scanSchedule(t.tx.QueryRow(`SELECT `+schedCols+` FROM schedules WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Schedule{}, store.ErrNotFound
	}
	return sc, err
}

// ListSchedules returns all schedules, newest first.
func (t *tx) ListSchedules() ([]store.Schedule, error) {
	rows, err := t.tx.Query(`SELECT ` + schedCols + ` FROM schedules ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Schedule
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// DeleteSchedule removes a schedule.
func (t *tx) DeleteSchedule(id string) error {
	_, err := t.tx.Exec(`DELETE FROM schedules WHERE id=?`, id)
	return err
}
