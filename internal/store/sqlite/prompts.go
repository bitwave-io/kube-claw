package sqlite

import (
	"database/sql"
	"errors"

	"github.com/traego/kube-claw/internal/store"
)

// SetPrompt creates or replaces an agent's system prompt.
func (t *tx) SetPrompt(p store.Prompt) error {
	if p.UpdatedAt == "" {
		p.UpdatedAt = store.NowRFC3339()
	}
	_, err := t.tx.Exec(
		`INSERT INTO prompts (agent_ns, agent_name, content, updated_at) VALUES (?,?,?,?)
		 ON CONFLICT(agent_ns, agent_name) DO UPDATE SET content=excluded.content, updated_at=excluded.updated_at`,
		p.AgentNamespace, p.AgentName, p.Content, p.UpdatedAt)
	return err
}

// GetPrompt returns an agent's prompt, or ErrNotFound.
func (t *tx) GetPrompt(ns, name string) (store.Prompt, error) {
	var p store.Prompt
	err := t.tx.QueryRow(
		`SELECT agent_ns, agent_name, content, updated_at FROM prompts WHERE agent_ns=? AND agent_name=?`,
		ns, name).Scan(&p.AgentNamespace, &p.AgentName, &p.Content, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Prompt{}, store.ErrNotFound
	}
	return p, err
}

// ListPrompts returns all stored prompts.
func (t *tx) ListPrompts() ([]store.Prompt, error) {
	rows, err := t.tx.Query(`SELECT agent_ns, agent_name, content, updated_at FROM prompts ORDER BY agent_ns, agent_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Prompt
	for rows.Next() {
		var p store.Prompt
		if err := rows.Scan(&p.AgentNamespace, &p.AgentName, &p.Content, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
