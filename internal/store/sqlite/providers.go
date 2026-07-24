package sqlite

import (
	"database/sql"
	"errors"

	"github.com/traego/kube-claw/internal/store"
)

// UpsertProvider creates or replaces a provider. An empty APIKeyCiphertext on
// update KEEPS the stored key (the UI never round-trips key material; blank
// means "unchanged"), mirroring UpsertModel.
func (t *tx) UpsertProvider(p store.Provider) error {
	now := store.NowRFC3339()
	_, err := t.tx.Exec(
		`INSERT INTO providers (name, kind, base_url, api_key_cipher, enabled, model_prefix, last_synced_at, last_sync_error, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET
		   kind=excluded.kind, base_url=excluded.base_url,
		   api_key_cipher=CASE WHEN length(excluded.api_key_cipher) > 0 THEN excluded.api_key_cipher ELSE providers.api_key_cipher END,
		   enabled=excluded.enabled, model_prefix=excluded.model_prefix, updated_at=excluded.updated_at`,
		p.Name, p.Kind, p.BaseURL, p.APIKeyCiphertext, boolInt(p.Enabled), p.ModelPrefix,
		p.LastSyncedAt, p.LastSyncError, now, now)
	return err
}

// GetProvider returns a provider by name, or ErrNotFound.
func (t *tx) GetProvider(name string) (store.Provider, error) {
	return t.scanProvider(t.tx.QueryRow(
		`SELECT name, kind, base_url, api_key_cipher, enabled, model_prefix, last_synced_at, last_sync_error, created_at, updated_at
		 FROM providers WHERE name=?`, name))
}

// ListProviders returns all providers, by name.
func (t *tx) ListProviders() ([]store.Provider, error) {
	rows, err := t.tx.Query(
		`SELECT name, kind, base_url, api_key_cipher, enabled, model_prefix, last_synced_at, last_sync_error, created_at, updated_at
		 FROM providers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Provider
	for rows.Next() {
		p, err := t.scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteProvider removes a provider and every model its catalog discovered
// (and those models' session pins) — deleting a provider revokes its models.
func (t *tx) DeleteProvider(name string) error {
	if _, err := t.tx.Exec(
		`DELETE FROM session_models WHERE model_name IN (SELECT name FROM models WHERE provider_name=?)`, name); err != nil {
		return err
	}
	if _, err := t.tx.Exec(`DELETE FROM models WHERE provider_name=?`, name); err != nil {
		return err
	}
	_, err := t.tx.Exec(`DELETE FROM providers WHERE name=?`, name)
	return err
}

// SetProviderSyncState records a catalog refresh outcome.
func (t *tx) SetProviderSyncState(name, syncedAt, syncErr string) error {
	res, err := t.tx.Exec(`UPDATE providers SET last_synced_at=?, last_sync_error=?, updated_at=? WHERE name=?`,
		syncedAt, syncErr, store.NowRFC3339(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (t *tx) scanProvider(r rowScanner) (store.Provider, error) {
	var p store.Provider
	var baseURL, prefix, syncedAt, syncErr sql.NullString
	var enabled int
	err := r.Scan(&p.Name, &p.Kind, &baseURL, &p.APIKeyCiphertext, &enabled, &prefix, &syncedAt, &syncErr, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Provider{}, store.ErrNotFound
	}
	if err != nil {
		return store.Provider{}, err
	}
	p.BaseURL, p.ModelPrefix = baseURL.String, prefix.String
	p.LastSyncedAt, p.LastSyncError, p.Enabled = syncedAt.String, syncErr.String, enabled == 1
	return p, nil
}
