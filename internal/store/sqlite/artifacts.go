package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/traego/kube-claw/internal/store"
)

// CreateArtifact stores a published document.
func (t *tx) CreateArtifact(a store.Artifact) error {
	if a.CreatedAt == "" {
		a.CreatedAt = store.NowRFC3339()
	}
	_, err := t.tx.Exec(
		`INSERT INTO artifacts (id, run_id, session_id, title, content, created_at) VALUES (?,?,?,?,?,?)`,
		a.ID, a.RunID, a.SessionID, a.Title, a.Content, a.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create artifact: %w", err)
	}
	return nil
}

// GetArtifact returns an artifact by id, or store.ErrNotFound.
func (t *tx) GetArtifact(id string) (store.Artifact, error) {
	var a store.Artifact
	var runID, sessionID sql.NullString
	err := t.tx.QueryRow(
		`SELECT id, run_id, session_id, title, content, created_at FROM artifacts WHERE id=?`, id,
	).Scan(&a.ID, &runID, &sessionID, &a.Title, &a.Content, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Artifact{}, store.ErrNotFound
	}
	if err != nil {
		return store.Artifact{}, err
	}
	a.RunID, a.SessionID = runID.String, sessionID.String
	return a, nil
}

// CreateArtifactToken stores a time-bound share token (caller passes the HASH).
func (t *tx) CreateArtifactToken(tokenHash, artifactID, expiresAt string) error {
	_, err := t.tx.Exec(
		`INSERT INTO artifact_tokens (token_hash, artifact_id, created_at, expires_at) VALUES (?,?,?,?)`,
		tokenHash, artifactID, store.NowRFC3339(), expiresAt,
	)
	if err != nil {
		return fmt.Errorf("create artifact token: %w", err)
	}
	return nil
}

// ResolveArtifactToken returns the artifact for a live (unexpired, unrevoked)
// token hash. Multi-read: resolving does NOT consume the token.
func (t *tx) ResolveArtifactToken(tokenHash string) (store.Artifact, string, error) {
	var artifactID, expiresAt string
	var revokedAt sql.NullString
	err := t.tx.QueryRow(
		`SELECT artifact_id, expires_at, revoked_at FROM artifact_tokens WHERE token_hash=?`, tokenHash,
	).Scan(&artifactID, &expiresAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Artifact{}, "", store.ErrNotFound
	}
	if err != nil {
		return store.Artifact{}, "", err
	}
	// Revoked and expired collapse to one error — no oracle distinguishing them.
	if revokedAt.Valid || expiresAt < store.NowRFC3339() { // RFC3339Nano sorts lexically by time
		return store.Artifact{}, expiresAt, store.ErrTokenExpired
	}
	a, err := t.GetArtifact(artifactID)
	if err != nil {
		return store.Artifact{}, "", err
	}
	return a, expiresAt, nil
}

// RevokeArtifactTokens revokes all live tokens for an artifact.
func (t *tx) RevokeArtifactTokens(artifactID string) error {
	_, err := t.tx.Exec(
		`UPDATE artifact_tokens SET revoked_at=? WHERE artifact_id=? AND revoked_at IS NULL`,
		store.NowRFC3339(), artifactID,
	)
	return err
}
