package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/traego/kube-claw/internal/store"
)

const gitRepoCols = `id, namespace, name, url, description, read_credential, write_credential, created_at`

// CreateGitRepo registers a repository and its granters.
func (t *tx) CreateGitRepo(g store.GitRepo) error {
	if g.CreatedAt == "" {
		g.CreatedAt = store.NowRFC3339()
	}
	_, err := t.tx.Exec(
		`INSERT INTO git_repos (`+gitRepoCols+`) VALUES (?,?,?,?,?,?,?,?)`,
		g.ID, g.Namespace, g.Name, g.URL, g.Description, g.ReadCredential, g.WriteCredential, g.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create git repo: %w", err)
	}
	for _, p := range g.Granters {
		if _, err := t.tx.Exec(`INSERT INTO git_repo_granters (repo_id, principal) VALUES (?,?)`, g.ID, p); err != nil {
			return fmt.Errorf("add git repo granter: %w", err)
		}
	}
	return nil
}

func scanGitRepo(s interface{ Scan(...any) error }) (store.GitRepo, error) {
	var g store.GitRepo
	var desc, readCred, writeCred sql.NullString
	err := s.Scan(&g.ID, &g.Namespace, &g.Name, &g.URL, &desc, &readCred, &writeCred, &g.CreatedAt)
	if err != nil {
		return g, err
	}
	g.Description, g.ReadCredential, g.WriteCredential = desc.String, readCred.String, writeCred.String
	return g, nil
}

// loadGitRepoGranters populates g.Granters.
func (t *tx) loadGitRepoGranters(g *store.GitRepo) error {
	rows, err := t.tx.Query(`SELECT principal FROM git_repo_granters WHERE repo_id=?`, g.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return err
		}
		g.Granters = append(g.Granters, p)
	}
	return rows.Err()
}

// GetGitRepo returns a repo by namespace/name (incl. granters).
func (t *tx) GetGitRepo(namespace, name string) (store.GitRepo, error) {
	g, err := scanGitRepo(t.tx.QueryRow(`SELECT `+gitRepoCols+` FROM git_repos WHERE namespace=? AND name=?`, namespace, name))
	if errors.Is(err, sql.ErrNoRows) {
		return store.GitRepo{}, store.ErrNotFound
	}
	if err != nil {
		return store.GitRepo{}, err
	}
	return g, t.loadGitRepoGranters(&g)
}

// GetGitRepoByID returns a repo by id (incl. granters).
func (t *tx) GetGitRepoByID(id string) (store.GitRepo, error) {
	g, err := scanGitRepo(t.tx.QueryRow(`SELECT `+gitRepoCols+` FROM git_repos WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.GitRepo{}, store.ErrNotFound
	}
	if err != nil {
		return store.GitRepo{}, err
	}
	return g, t.loadGitRepoGranters(&g)
}

// ListGitRepos returns all repos (never credentials), newest first.
func (t *tx) ListGitRepos() ([]store.GitRepo, error) {
	rows, err := t.tx.Query(`SELECT ` + gitRepoCols + ` FROM git_repos ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.GitRepo
	for rows.Next() {
		g, err := scanGitRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := t.loadGitRepoGranters(&out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteGitRepo removes a repo and its granters, grants, and requests.
func (t *tx) DeleteGitRepo(namespace, name string) error {
	g, err := t.GetGitRepo(namespace, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM git_repo_granters WHERE repo_id=?`,
		`DELETE FROM git_repo_grants WHERE repo_id=?`,
		`DELETE FROM git_repo_requests WHERE repo_id=?`,
		`DELETE FROM git_repos WHERE id=?`,
	} {
		if _, err := t.tx.Exec(q, g.ID); err != nil {
			return fmt.Errorf("delete git repo: %w", err)
		}
	}
	return nil
}

// CreateGitRepoGrant stores a durable git-repo grant.
func (t *tx) CreateGitRepoGrant(g store.GitRepoGrant) error {
	if g.ApprovedAt == "" {
		g.ApprovedAt = store.NowRFC3339()
	}
	_, err := t.tx.Exec(
		`INSERT INTO git_repo_grants (id, agent_ns, agent_name, service_account, image_digest,
		   agent_spec_hash, repo_id, access, approved_by, approved_at, reason)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.AgentNamespace, g.AgentName, g.ServiceAccount, g.ImageDigest,
		g.AgentSpecHash, g.GitRepoID, g.Access, g.ApprovedBy, g.ApprovedAt, g.Reason,
	)
	if err != nil {
		return fmt.Errorf("create git repo grant: %w", err)
	}
	return nil
}

// FindValidGitRepoGrant returns a non-revoked grant matching the binding. When
// both a read and a write grant exist, the highest access (write) is returned
// so write-implies-read holds without the caller merging rows.
func (t *tx) FindValidGitRepoGrant(ns, agent, repoID, digest, specHash string) (store.GitRepoGrant, error) {
	var g store.GitRepoGrant
	var sa, reason sql.NullString
	err := t.tx.QueryRow(
		`SELECT id, agent_ns, agent_name, service_account, image_digest, agent_spec_hash,
		        repo_id, access, approved_by, approved_at, reason
		 FROM git_repo_grants
		 WHERE agent_ns=? AND agent_name=? AND repo_id=? AND image_digest=?
		   AND agent_spec_hash=? AND revoked_at IS NULL
		 ORDER BY CASE access WHEN 'write' THEN 0 ELSE 1 END
		 LIMIT 1`,
		ns, agent, repoID, digest, specHash,
	).Scan(&g.ID, &g.AgentNamespace, &g.AgentName, &sa, &g.ImageDigest, &g.AgentSpecHash,
		&g.GitRepoID, &g.Access, &g.ApprovedBy, &g.ApprovedAt, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return store.GitRepoGrant{}, store.ErrNotFound
	}
	if err != nil {
		return store.GitRepoGrant{}, err
	}
	g.ServiceAccount, g.Reason = sa.String, reason.String
	return g, nil
}

// RevokeGitRepoGrant marks a git-repo grant revoked.
func (t *tx) RevokeGitRepoGrant(id, reason string) error {
	res, err := t.tx.Exec(
		`UPDATE git_repo_grants SET revoked_at=?, revoked_reason=? WHERE id=? AND revoked_at IS NULL`,
		store.NowRFC3339(), reason, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ListGitRepoGrants returns git-repo grants for an agent (newest first).
func (t *tx) ListGitRepoGrants(ns, agent string) ([]store.GitRepoGrant, error) {
	rows, err := t.tx.Query(
		`SELECT id, agent_ns, agent_name, image_digest, agent_spec_hash, repo_id, access,
		        approved_by, approved_at, COALESCE(revoked_at,'')
		 FROM git_repo_grants WHERE agent_ns=? AND agent_name=? ORDER BY approved_at DESC`,
		ns, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.GitRepoGrant
	for rows.Next() {
		var g store.GitRepoGrant
		if err := rows.Scan(&g.ID, &g.AgentNamespace, &g.AgentName, &g.ImageDigest,
			&g.AgentSpecHash, &g.GitRepoID, &g.Access, &g.ApprovedBy, &g.ApprovedAt, &g.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CreateGitRepoRequest stores a pending git-repo access request.
func (t *tx) CreateGitRepoRequest(req store.GitRepoRequest) error {
	if req.CreatedAt == "" {
		req.CreatedAt = store.NowRFC3339()
	}
	if req.Status == "" {
		req.Status = "Pending"
	}
	_, err := t.tx.Exec(
		`INSERT INTO git_repo_requests (id, status, agent_ns, agent_name, run_id, repo_id,
		   repo_name, access, image_digest, context, requested_by, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		req.ID, req.Status, req.AgentNamespace, req.AgentName, req.RunID, req.GitRepoID,
		req.RepoName, req.Access, req.ImageDigest, req.Context, req.RequestedBy, req.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("create git repo request: %w", err)
	}
	return nil
}

const gitReqCols = `id, status, agent_ns, agent_name, run_id, repo_id, repo_name, access, image_digest, context, requested_by, created_at, notified_at`

func scanGitReq(s interface{ Scan(...any) error }) (store.GitRepoRequest, error) {
	var r store.GitRepoRequest
	var runID, repoName, digest, ctx, requestedBy, notified sql.NullString
	err := s.Scan(&r.ID, &r.Status, &r.AgentNamespace, &r.AgentName, &runID, &r.GitRepoID,
		&repoName, &r.Access, &digest, &ctx, &requestedBy, &r.CreatedAt, &notified)
	if err != nil {
		return r, err
	}
	r.RunID, r.RepoName, r.ImageDigest, r.Context = runID.String, repoName.String, digest.String, ctx.String
	r.RequestedBy, r.NotifiedAt = requestedBy.String, notified.String
	return r, nil
}

// GetGitRepoRequest returns a request by id.
func (t *tx) GetGitRepoRequest(id string) (store.GitRepoRequest, error) {
	r, err := scanGitReq(t.tx.QueryRow(`SELECT `+gitReqCols+` FROM git_repo_requests WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.GitRepoRequest{}, store.ErrNotFound
	}
	return r, err
}

// GetPendingGitRepoRequest returns the Pending request for an agent+repo at the
// given access level.
func (t *tx) GetPendingGitRepoRequest(ns, agent, repoID, access string) (store.GitRepoRequest, error) {
	r, err := scanGitReq(t.tx.QueryRow(
		`SELECT `+gitReqCols+` FROM git_repo_requests
		 WHERE agent_ns=? AND agent_name=? AND repo_id=? AND access=? AND status='Pending'
		 ORDER BY created_at ASC LIMIT 1`, ns, agent, repoID, access))
	if errors.Is(err, sql.ErrNoRows) {
		return store.GitRepoRequest{}, store.ErrNotFound
	}
	return r, err
}

// ListGitRepoRequests returns requests with the given status (all if "").
func (t *tx) ListGitRepoRequests(status string) ([]store.GitRepoRequest, error) {
	q := `SELECT ` + gitReqCols + ` FROM git_repo_requests`
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
	var out []store.GitRepoRequest
	for rows.Next() {
		r, err := scanGitReq(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetGitRepoRequestStatus updates a request's status.
func (t *tx) SetGitRepoRequestStatus(id, status string) error {
	res, err := t.tx.Exec(`UPDATE git_repo_requests SET status=? WHERE id=?`, status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}
