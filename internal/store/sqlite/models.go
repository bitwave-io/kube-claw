package sqlite

import (
	"database/sql"
	"errors"

	"github.com/traego/kube-claw/internal/store"
)

// UpsertModel creates or replaces a named model configuration. An empty
// APIKeyCiphertext on update KEEPS the stored key (the UI never round-trips
// key material; blank means "unchanged").
func (t *tx) UpsertModel(m store.Model) error {
	_, err := t.tx.Exec(
		`INSERT INTO models (name, provider, model_id, base_url, api_key_cipher, notes, max_tokens, is_default, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET
		   provider=excluded.provider, model_id=excluded.model_id,
		   base_url=excluded.base_url,
		   api_key_cipher=CASE WHEN length(excluded.api_key_cipher) > 0 THEN excluded.api_key_cipher ELSE models.api_key_cipher END,
		   notes=excluded.notes, max_tokens=excluded.max_tokens, updated_at=excluded.updated_at`,
		m.Name, m.Provider, m.ModelID, m.BaseURL, m.APIKeyCiphertext, m.Notes, m.MaxTokens, boolInt(m.IsDefault), store.NowRFC3339())
	return err
}

// GetModel returns a model by name, or ErrNotFound.
func (t *tx) GetModel(name string) (store.Model, error) {
	return t.scanModel(t.tx.QueryRow(
		`SELECT name, provider, model_id, base_url, api_key_cipher, notes, max_tokens, is_default, updated_at
		 FROM models WHERE name=?`, name))
}

// ListModels returns all configured models, default first then by name.
func (t *tx) ListModels() ([]store.Model, error) {
	rows, err := t.tx.Query(
		`SELECT name, provider, model_id, base_url, api_key_cipher, notes, max_tokens, is_default, updated_at
		 FROM models ORDER BY is_default DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Model
	for rows.Next() {
		m, err := t.scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteModel removes a model and any session overrides pointing at it.
func (t *tx) DeleteModel(name string) error {
	if _, err := t.tx.Exec(`DELETE FROM session_models WHERE model_name=?`, name); err != nil {
		return err
	}
	_, err := t.tx.Exec(`DELETE FROM models WHERE name=?`, name)
	return err
}

// SetDefaultModel marks one model as the install default (clears others).
// Existence is checked FIRST: the update touches every row, and a failed
// set-default must not wipe the current default on its way to ErrNotFound.
func (t *tx) SetDefaultModel(name string) error {
	var n int
	if err := t.tx.QueryRow(`SELECT COUNT(*) FROM models WHERE name=?`, name).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	_, err := t.tx.Exec(`UPDATE models SET is_default = (name=?)`, name)
	return err
}

// GetDefaultModel returns the default model, or ErrNotFound when none is set.
func (t *tx) GetDefaultModel() (store.Model, error) {
	return t.scanModel(t.tx.QueryRow(
		`SELECT name, provider, model_id, base_url, api_key_cipher, notes, max_tokens, is_default, updated_at
		 FROM models WHERE is_default=1`))
}

// SetSessionModel pins a session (Slack thread) to a named model.
func (t *tx) SetSessionModel(sessionID, modelName, setBy string) error {
	_, err := t.tx.Exec(
		`INSERT INTO session_models (session_id, model_name, set_by, set_at) VALUES (?,?,?,?)
		 ON CONFLICT(session_id) DO UPDATE SET model_name=excluded.model_name, set_by=excluded.set_by, set_at=excluded.set_at`,
		sessionID, modelName, setBy, store.NowRFC3339())
	return err
}

// GetSessionModel returns the session's pinned model name, or ErrNotFound.
func (t *tx) GetSessionModel(sessionID string) (string, error) {
	var name string
	err := t.tx.QueryRow(`SELECT model_name FROM session_models WHERE session_id=?`, sessionID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	return name, err
}

type rowScanner interface{ Scan(dest ...any) error }

func (t *tx) scanModel(r rowScanner) (store.Model, error) {
	var m store.Model
	var baseURL, notes sql.NullString
	var isDefault int
	err := r.Scan(&m.Name, &m.Provider, &m.ModelID, &baseURL, &m.APIKeyCiphertext, &notes, &m.MaxTokens, &isDefault, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Model{}, store.ErrNotFound
	}
	if err != nil {
		return store.Model{}, err
	}
	m.BaseURL, m.Notes, m.IsDefault = baseURL.String, notes.String, isDefault == 1
	return m, nil
}
