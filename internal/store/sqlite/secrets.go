package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/traego/kube-claw/internal/store"
)

// CreateSecret inserts secret metadata + its granters.
func (t *tx) CreateSecret(s store.Secret) error {
	if s.CreatedAt == "" {
		s.CreatedAt = store.NowRFC3339()
	}
	if _, err := t.tx.Exec(
		`INSERT INTO secrets (id, namespace, name, type, description, created_at) VALUES (?,?,?,?,?,?)`,
		s.ID, s.Namespace, s.Name, s.Type, s.Description, s.CreatedAt,
	); err != nil {
		return fmt.Errorf("create secret: %w", err)
	}
	for _, g := range s.Granters {
		if _, err := t.tx.Exec(
			`INSERT INTO secret_granters (secret_id, principal) VALUES (?,?)`, s.ID, g,
		); err != nil {
			return fmt.Errorf("add granter: %w", err)
		}
	}
	return nil
}

// GetSecret returns secret metadata (with granters) by namespace/name.
func (t *tx) GetSecret(namespace, name string) (store.Secret, error) {
	var s store.Secret
	var typ, desc sql.NullString
	err := t.tx.QueryRow(
		`SELECT id, namespace, name, type, description, created_at FROM secrets WHERE namespace=? AND name=?`,
		namespace, name,
	).Scan(&s.ID, &s.Namespace, &s.Name, &typ, &desc, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Secret{}, store.ErrNotFound
	}
	if err != nil {
		return store.Secret{}, err
	}
	s.Type, s.Description = typ.String, desc.String

	rows, err := t.tx.Query(`SELECT principal FROM secret_granters WHERE secret_id=?`, s.ID)
	if err != nil {
		return store.Secret{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return store.Secret{}, err
		}
		s.Granters = append(s.Granters, p)
	}
	return s, rows.Err()
}

// AddSecretVersion stores a new encrypted version.
func (t *tx) AddSecretVersion(v store.SecretVersion) error {
	if v.CreatedAt == "" {
		v.CreatedAt = store.NowRFC3339()
	}
	_, err := t.tx.Exec(
		`INSERT INTO secret_versions (id, secret_id, ciphertext, checksum, created_at, created_by)
		 VALUES (?,?,?,?,?,?)`,
		v.ID, v.SecretID, v.Ciphertext, v.Checksum, v.CreatedAt, v.CreatedBy,
	)
	if err != nil {
		return fmt.Errorf("add secret version: %w", err)
	}
	return nil
}

// LatestSecretVersion returns the newest version of a secret.
func (t *tx) LatestSecretVersion(secretID string) (store.SecretVersion, error) {
	var v store.SecretVersion
	var checksum, createdBy sql.NullString
	err := t.tx.QueryRow(
		`SELECT id, secret_id, ciphertext, checksum, created_at, created_by
		 FROM secret_versions WHERE secret_id=? ORDER BY created_at DESC, id DESC LIMIT 1`,
		secretID,
	).Scan(&v.ID, &v.SecretID, &v.Ciphertext, &checksum, &v.CreatedAt, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return store.SecretVersion{}, store.ErrNotFound
	}
	if err != nil {
		return store.SecretVersion{}, err
	}
	v.Checksum, v.CreatedBy = checksum.String, createdBy.String
	return v, nil
}

// CreateIntakeToken stores a one-time intake token (caller passes the HASH).
func (t *tx) CreateIntakeToken(tokenHash, secretID, expiresAt string) error {
	_, err := t.tx.Exec(
		`INSERT INTO intake_tokens (token_hash, secret_id, created_at, expires_at) VALUES (?,?,?,?)`,
		tokenHash, secretID, store.NowRFC3339(), expiresAt,
	)
	if err != nil {
		return fmt.Errorf("create intake token: %w", err)
	}
	return nil
}

// ConsumeIntakeToken validates + single-use-consumes a token by hash.
func (t *tx) ConsumeIntakeToken(tokenHash string) (string, error) {
	var secretID, expiresAt string
	var consumedAt sql.NullString
	err := t.tx.QueryRow(
		`SELECT secret_id, expires_at, consumed_at FROM intake_tokens WHERE token_hash=?`, tokenHash,
	).Scan(&secretID, &expiresAt, &consumedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", store.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if consumedAt.Valid {
		return "", store.ErrTokenUsed
	}
	if expiresAt < store.NowRFC3339() { // RFC3339Nano sorts lexically by time
		return "", store.ErrTokenExpired
	}
	if _, err := t.tx.Exec(
		`UPDATE intake_tokens SET consumed_at=? WHERE token_hash=?`, store.NowRFC3339(), tokenHash,
	); err != nil {
		return "", err
	}
	return secretID, nil
}
