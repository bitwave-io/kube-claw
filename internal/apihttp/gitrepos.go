package apihttp

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/traego/kube-claw/internal/gitrepo"
	slackrouter "github.com/traego/kube-claw/internal/router/slack"
	"github.com/traego/kube-claw/internal/store"
)

// Git-repo plane: a repository is registered by an admin (URL + read/write
// credentials + granters); an agent then requests access to it BY NAME at a
// level (read | write) at runtime. The request goes to the repo's granters for
// approval and becomes a durable grant bound to the agent's current image
// digest + spec hash + access level — mirroring the secret request→approve→grant
// flow (ondemand.go). A read grant materializes the read credential; a write
// grant materializes the write credential (write implies read).
//
// Management endpoints (create/list/delete) are admin-gated. Approve/deny/revoke
// share the /v1 surface with the secret equivalents. The runtime endpoints
// (available/request/requested) are token-scoped to the calling run.

type createGitRepoReq struct {
	Name            string   `json:"name"`
	Namespace       string   `json:"namespace"`
	URL             string   `json:"url"`
	Description     string   `json:"description"`
	ReadCredential  string   `json:"readCredential"`
	WriteCredential string   `json:"writeCredential"`
	Granters        []string `json:"granters"`
}

// createGitRepo registers a repository. Credentials are write-only: they are
// stored and handed to granted agents, but never returned or listed.
func (s *Server) createGitRepo(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeErr(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	var req createGitRepoReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.URL = strings.TrimSpace(req.URL)
	if req.Name == "" || req.URL == "" {
		writeErr(w, http.StatusBadRequest, "name and url are required")
		return
	}
	if !gitrepo.ValidRepoName(req.Name) {
		writeErr(w, http.StatusBadRequest, "invalid repo name")
		return
	}
	if req.ReadCredential == "" && req.WriteCredential == "" {
		writeErr(w, http.StatusBadRequest, "at least one of readCredential or writeCredential is required")
		return
	}
	if req.Namespace == "" {
		req.Namespace = "claw-agents"
	}
	repo := store.GitRepo{
		ID:              gitrepo.NewGitRepoID(),
		Namespace:       req.Namespace,
		Name:            req.Name,
		URL:             req.URL,
		Description:     req.Description,
		ReadCredential:  req.ReadCredential,
		WriteCredential: req.WriteCredential,
		Granters:        req.Granters,
	}
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if err := tx.CreateGitRepo(repo); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "gitrepo.created", Actor: "admin",
			Detail: map[string]any{"repo": repo.ID, "name": repo.Name, "namespace": repo.Namespace}})
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":        repo.ID,
		"name":      repo.Name,
		"namespace": repo.Namespace,
		"url":       repo.URL,
	})
}

func (s *Server) listGitRepos(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeErr(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	var repos []store.GitRepo
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListGitRepos()
		repos = got
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if repos == nil {
		repos = []store.GitRepo{}
	}
	writeJSON(w, http.StatusOK, repos) // credentials are json:"-"
}

func (s *Server) deleteGitRepo(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeErr(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		ns = "claw-agents"
	}
	name := r.PathValue("name")
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if err := tx.DeleteGitRepo(ns, name); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "gitrepo.deleted", Actor: "admin",
			Detail: map[string]any{"namespace": ns, "name": name}})
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- approval / grants (admin/CLI surface, mirrors secret equivalents) ---

func (s *Server) listGitRepoRequests(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	var reqs []store.GitRepoRequest
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListGitRepoRequests(status)
		reqs = got
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if reqs == nil {
		reqs = []store.GitRepoRequest{}
	}
	writeJSON(w, http.StatusOK, reqs)
}

func (s *Server) approveGitRepoRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body approveReq
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Approver == "" {
		body.Approver = "cli"
	}
	grant, err := s.GitRepos.Approve(r.Context(), id, body.Approver, body.Reason)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "request not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"grant": grant.ID, "status": "approved"})
}

func (s *Server) denyGitRepoRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body approveReq
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.GitRepos.Deny(r.Context(), id, body.Approver, body.Reason); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "request not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "denied"})
}

func (s *Server) listGitRepoGrants(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	agent := r.URL.Query().Get("agent")
	var grants []store.GitRepoGrant
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListGitRepoGrants(ns, agent)
		grants = got
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if grants == nil {
		grants = []store.GitRepoGrant{}
	}
	writeJSON(w, http.StatusOK, grants)
}

func (s *Server) revokeGitRepoGrant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body approveReq
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.GitRepos.RevokeGrant(r.Context(), id, body.Approver, body.Reason); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "grant not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// --- runtime (token-scoped to the calling run, mirrors ondemand.go) ---

// availableGitRepos lists the repos (name + url + description, never
// credentials) in the run's namespace, so the agent knows what it can request.
func (s *Server) availableGitRepos(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if claims, err := s.Signer.Verify(bearer(r)); err != nil || claims.RunID != runID {
		writeErr(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	var run store.Run
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetRun(runID)
		run = got
		return e
	}); err != nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	type repo struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		Description string `json:"description"`
	}
	out := []repo{}
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		all, e := tx.ListGitRepos()
		if e != nil {
			return e
		}
		for _, x := range all {
			if x.Namespace == run.AgentNamespace {
				out = append(out, repo{Name: x.Name, URL: x.URL, Description: x.Description})
			}
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]any{"repos": out})
}

// requestGitRepoReq is what the agent's request_gitrepo tool sends.
type requestGitRepoReq struct {
	Name   string `json:"name"`
	Access string `json:"access"` // read|write (default read)
	Reason string `json:"reason"` // justification shown to approvers
}

// requestGitRepo handles an agent's on-demand repo access request:
//   - agent already holds a grant covering the level → no-op (retrieves next poll).
//   - otherwise → open a GitRepoRequest and notify the repo's GRANTERS with the
//     agent's reason + who it's for (PAM access flow).
//
// Unlike secrets, repos are not self-provisioned by agents: the credential must
// be registered by an admin, so an unknown repo is a 404 rather than a create.
func (s *Server) requestGitRepo(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if claims, err := s.Signer.Verify(bearer(r)); err != nil || claims.RunID != runID {
		writeErr(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	var req requestGitRepoReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if !gitrepo.ValidRepoName(req.Name) {
		writeErr(w, http.StatusBadRequest, "invalid repo name")
		return
	}
	if req.Access == "" {
		req.Access = gitrepo.AccessRead
	}
	if !gitrepo.ValidAccess(req.Access) {
		writeErr(w, http.StatusBadRequest, "access must be 'read' or 'write'")
		return
	}
	var run store.Run
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetRun(runID)
		run = got
		return e
	}); err != nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	ns, agentName := run.AgentNamespace, run.AgentName
	user := slackrouter.SlackUser(run.Source)
	digest, specHash := s.agentBinding(r.Context(), ns, agentName)

	var repo store.GitRepo
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetGitRepo(ns, req.Name)
		repo = got
		return e
	}); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "repo not found: "+req.Name)
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.Access == gitrepo.AccessWrite && !repo.HasWriteCredential() {
		writeErr(w, http.StatusBadRequest, "repo has no write credential registered")
		return
	}

	// Already granted at a level that covers the request → nothing to do.
	granted := false
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if g, e := tx.FindValidGitRepoGrant(ns, agentName, repo.ID, digest, specHash); e == nil {
			granted = gitrepo.Satisfies(g.Access, req.Access)
		}
		return nil
	})
	if granted {
		writeJSON(w, http.StatusOK, map[string]string{"status": "granted"})
		return
	}

	// Open (or dedupe) an access request to the repo's granters.
	var reqID string
	isNew := false
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if existing, e := tx.GetPendingGitRepoRequest(ns, agentName, repo.ID, req.Access); e == nil {
			reqID = existing.ID
			return nil
		}
		reqID = gitrepo.NewID("grepo-req")
		if e := tx.CreateGitRepoRequest(store.GitRepoRequest{
			ID: reqID, Status: "Pending", AgentNamespace: ns, AgentName: agentName, RunID: runID,
			GitRepoID: repo.ID, RepoName: req.Name, Access: req.Access, ImageDigest: digest,
			Context: req.Reason, RequestedBy: user,
		}); e != nil {
			return e
		}
		isNew = true
		return tx.AppendAudit(store.AuditEvent{Type: "gitrepo.access_requested", RunID: runID,
			Actor: agentName, Detail: map[string]any{"repo": req.Name, "access": req.Access, "requestedBy": user, "reason": req.Reason}})
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if isNew && s.Notifier != nil {
		msg := fmt.Sprintf(":lock: *Git repo access request*\nAgent `%s` is requesting *%s* access to repo *%s*.", agentName, req.Access, req.Name)
		if user != "" {
			msg += fmt.Sprintf("\n• For: <@%s>", user)
		}
		if req.Reason != "" {
			msg += "\n• Why: " + req.Reason
		}
		msg += fmt.Sprintf("\n\nApprove with:\n`POST /v1/gitrepo-requests/%s/approve`", reqID)
		for _, g := range repo.Granters {
			_ = s.Notifier.PostReply(r.Context(), g, "", msg)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "access_requested"})
}

// requestedGitRepo returns the repo URL + the credential for the agent's granted
// access level, once it holds a valid grant. A write grant returns the write
// credential; a read grant returns the read credential (write implies read).
func (s *Server) requestedGitRepo(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if claims, err := s.Signer.Verify(bearer(r)); err != nil || claims.RunID != runID {
		writeErr(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" || !gitrepo.ValidRepoName(name) {
		writeErr(w, http.StatusBadRequest, "invalid or missing repo name")
		return
	}
	var run store.Run
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetRun(runID)
		run = got
		return e
	}); err != nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	ns, agentName := run.AgentNamespace, run.AgentName
	digest, specHash := s.agentBinding(r.Context(), ns, agentName)

	var repo store.GitRepo
	var grant store.GitRepoGrant
	granted := false
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetGitRepo(ns, name)
		if e != nil {
			return nil
		}
		repo = got
		if g, ge := tx.FindValidGitRepoGrant(ns, agentName, repo.ID, digest, specHash); ge == nil {
			grant, granted = g, true
		}
		return nil
	})
	if !granted {
		w.WriteHeader(http.StatusNoContent) // not granted yet — caller polls
		return
	}
	// Hand back the credential matching the granted level. Write implies read,
	// so a write grant materializes the write credential.
	cred := repo.ReadCredential
	if grant.Access == gitrepo.AccessWrite {
		cred = repo.WriteCredential
	}
	if cred == "" {
		w.WriteHeader(http.StatusNoContent) // granted but no credential registered for this level
		return
	}
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		return tx.AppendAudit(store.AuditEvent{Type: "gitrepo.materialized", RunID: runID, GrantID: grant.ID,
			Detail: map[string]any{"repo": name, "access": grant.Access}})
	})
	writeJSON(w, http.StatusOK, map[string]string{
		"name":       name,
		"url":        repo.URL,
		"access":     grant.Access,
		"credential": base64.StdEncoding.EncodeToString([]byte(cred)),
	})
}
