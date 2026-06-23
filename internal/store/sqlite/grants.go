package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/traego/kube-claw/internal/store"
)

// CreateGrant stores a durable grant.
func (t *tx) CreateGrant(g store.Grant) error {
	if g.ApprovedAt == "" {
		g.ApprovedAt = store.NowRFC3339()
	}
	_, err := t.tx.Exec(
		`INSERT INTO grants (id, agent_ns, agent_name, service_account, image_digest,
		   agent_spec_hash, secret_id, delivery_hash, approved_by, approved_at, reason)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.AgentNamespace, g.AgentName, g.ServiceAccount, g.ImageDigest,
		g.AgentSpecHash, g.SecretID, g.DeliveryHash, g.ApprovedBy, g.ApprovedAt, g.Reason,
	)
	if err != nil {
		return fmt.Errorf("create grant: %w", err)
	}
	return nil
}

// FindValidGrant returns a non-revoked grant matching the full binding.
func (t *tx) FindValidGrant(ns, agent, secretID, digest, specHash, deliveryHash string) (store.Grant, error) {
	var g store.Grant
	var sa, reason sql.NullString
	err := t.tx.QueryRow(
		`SELECT id, agent_ns, agent_name, service_account, image_digest, agent_spec_hash,
		        secret_id, delivery_hash, approved_by, approved_at, reason
		 FROM grants
		 WHERE agent_ns=? AND agent_name=? AND secret_id=? AND image_digest=?
		   AND agent_spec_hash=? AND delivery_hash=? AND revoked_at IS NULL
		 LIMIT 1`,
		ns, agent, secretID, digest, specHash, deliveryHash,
	).Scan(&g.ID, &g.AgentNamespace, &g.AgentName, &sa, &g.ImageDigest, &g.AgentSpecHash,
		&g.SecretID, &g.DeliveryHash, &g.ApprovedBy, &g.ApprovedAt, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Grant{}, store.ErrNotFound
	}
	if err != nil {
		return store.Grant{}, err
	}
	g.ServiceAccount, g.Reason = sa.String, reason.String
	return g, nil
}

// RevokeGrant marks a grant revoked.
func (t *tx) RevokeGrant(id, reason string) error {
	res, err := t.tx.Exec(
		`UPDATE grants SET revoked_at=?, revoked_reason=? WHERE id=? AND revoked_at IS NULL`,
		store.NowRFC3339(), reason, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListGrants returns grants for an agent (newest first).
func (t *tx) ListGrants(ns, agent string) ([]store.Grant, error) {
	rows, err := t.tx.Query(
		`SELECT id, agent_ns, agent_name, image_digest, agent_spec_hash, secret_id,
		        delivery_hash, approved_by, approved_at, COALESCE(revoked_at,'')
		 FROM grants WHERE agent_ns=? AND agent_name=? ORDER BY approved_at DESC`,
		ns, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Grant
	for rows.Next() {
		var g store.Grant
		if err := rows.Scan(&g.ID, &g.AgentNamespace, &g.AgentName, &g.ImageDigest,
			&g.AgentSpecHash, &g.SecretID, &g.DeliveryHash, &g.ApprovedBy, &g.ApprovedAt, &g.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CreateSecretRequest stores a pending approval request.
func (t *tx) CreateSecretRequest(req store.SecretRequest) error {
	if req.CreatedAt == "" {
		req.CreatedAt = store.NowRFC3339()
	}
	if req.Status == "" {
		req.Status = "Pending"
	}
	_, err := t.tx.Exec(
		`INSERT INTO secret_requests (id, status, agent_ns, agent_name, run_id, secret_id,
		   secret_name, image_digest, context, requested_by, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		req.ID, req.Status, req.AgentNamespace, req.AgentName, req.RunID, req.SecretID,
		req.SecretName, req.ImageDigest, req.Context, req.RequestedBy, req.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create secret request: %w", err)
	}
	return nil
}

const reqCols = `id, status, agent_ns, agent_name, run_id, secret_id, secret_name, image_digest, context, requested_by, created_at, notified_at`

func scanReq(s interface{ Scan(...any) error }) (store.SecretRequest, error) {
	var r store.SecretRequest
	var runID, secretName, digest, ctx, requestedBy, notified sql.NullString
	err := s.Scan(&r.ID, &r.Status, &r.AgentNamespace, &r.AgentName, &runID, &r.SecretID,
		&secretName, &digest, &ctx, &requestedBy, &r.CreatedAt, &notified)
	if err != nil {
		return r, err
	}
	r.RunID, r.SecretName, r.ImageDigest, r.Context = runID.String, secretName.String, digest.String, ctx.String
	r.RequestedBy, r.NotifiedAt = requestedBy.String, notified.String
	return r, nil
}

// GetPendingRequest returns the Pending request for an agent+secret, or ErrNotFound.
func (t *tx) GetPendingRequest(ns, agent, secretID string) (store.SecretRequest, error) {
	r, err := scanReq(t.tx.QueryRow(
		`SELECT `+reqCols+` FROM secret_requests
		 WHERE agent_ns=? AND agent_name=? AND secret_id=? AND status='Pending'
		 ORDER BY created_at ASC LIMIT 1`, ns, agent, secretID))
	if errors.Is(err, sql.ErrNoRows) {
		return store.SecretRequest{}, store.ErrNotFound
	}
	return r, err
}

// MarkRequestNotified records that the approval was posted to Slack.
func (t *tx) MarkRequestNotified(id string) error {
	_, err := t.tx.Exec(`UPDATE secret_requests SET notified_at=? WHERE id=?`, store.NowRFC3339(), id)
	return err
}

func (t *tx) GetSecretRequest(id string) (store.SecretRequest, error) {
	r, err := scanReq(t.tx.QueryRow(`SELECT `+reqCols+` FROM secret_requests WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.SecretRequest{}, store.ErrNotFound
	}
	return r, err
}

func (t *tx) ListSecretRequests(status string) ([]store.SecretRequest, error) {
	q := `SELECT ` + reqCols + ` FROM secret_requests`
	var args []any
	if status != "" {
		q += ` WHERE status=?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := t.tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.SecretRequest
	for rows.Next() {
		r, err := scanReq(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (t *tx) PendingRequestExists(ns, agent, secretID string) (bool, error) {
	var n int
	err := t.tx.QueryRow(
		`SELECT count(*) FROM secret_requests WHERE agent_ns=? AND agent_name=? AND secret_id=? AND status='Pending'`,
		ns, agent, secretID).Scan(&n)
	return n > 0, err
}

func (t *tx) SetSecretRequestStatus(id, status string) error {
	res, err := t.tx.Exec(`UPDATE secret_requests SET status=? WHERE id=?`, status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}
