package sqlite

import (
	"database/sql"
	"errors"

	"github.com/traego/kube-claw/internal/store"
)

// SetSetting creates or replaces an install-wide setting.
func (t *tx) SetSetting(key, value string) error {
	_, err := t.tx.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, store.NowRFC3339())
	return err
}

// GetSetting returns a setting's value, or ErrNotFound.
func (t *tx) GetSetting(key string) (string, error) {
	var v string
	err := t.tx.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	return v, err
}

// SetSettingIfUnset stores a setting only when the key has no value yet
// (first-claim-wins, e.g. the upgrade admin claimed during onboarding).
// Returns true if this call set it.
func (t *tx) SetSettingIfUnset(key, value string) (bool, error) {
	res, err := t.tx.Exec(
		`INSERT INTO settings (key, value, updated_at) VALUES (?,?,?)
		 ON CONFLICT(key) DO NOTHING`,
		key, value, store.NowRFC3339())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
