package gitrepo

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/store"
)

// ErrNotGranter is returned when a principal is not authorized to approve a
// git-repo access request.
var ErrNotGranter = errors.New("principal is not a granter for this repo")

// Service is the git-repo approval authority. It mirrors internal/approvals for
// the gitrepo plane: the API/CLI path (Approve) is break-glass; the Slack path
// (ApproveByPrincipal) requires the principal to be one of the repo's granters.
// Both bind the grant to the agent's CURRENT image digest + spec hash and to
// the requested access level.
type Service struct {
	Store  store.Store
	Reader client.Reader
}

// Approve is the break-glass path: the caller is already trusted (authenticated
// API/CLI). It creates a grant from a pending request and resumes the run.
func (s *Service) Approve(ctx context.Context, reqID, approver, reason string) (store.GitRepoGrant, error) {
	return s.approve(ctx, reqID, approver, reason, nil)
}

// ApproveByPrincipal is the Slack path: principal MUST be a granter of the repo.
func (s *Service) ApproveByPrincipal(ctx context.Context, reqID, principal, reason string) (store.GitRepoGrant, error) {
	return s.approve(ctx, reqID, principal, reason, &principal)
}

func (s *Service) approve(ctx context.Context, reqID, approver, reason string, principal *string) (store.GitRepoGrant, error) {
	var req store.GitRepoRequest
	if err := s.Store.Tx(ctx, func(tx store.Tx) error {
		got, e := tx.GetGitRepoRequest(reqID)
		req = got
		return e
	}); err != nil {
		return store.GitRepoGrant{}, err
	}

	if principal != nil {
		ok, err := s.isGranter(ctx, req, *principal)
		if err != nil {
			return store.GitRepoGrant{}, err
		}
		if !ok {
			return store.GitRepoGrant{}, ErrNotGranter
		}
	}

	// Bind to the agent's CURRENT state (approve what is current, DESIGN.md §8).
	var agent clawv1alpha1.Agent
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: req.AgentNamespace, Name: req.AgentName}, &agent); err != nil {
		return store.GitRepoGrant{}, fmt.Errorf("load agent: %w", err)
	}

	var grant store.GitRepoGrant
	err := s.Store.Tx(ctx, func(tx store.Tx) error {
		cur, e := tx.GetGitRepoRequest(reqID)
		if e != nil {
			return e
		}
		if cur.Status != "Pending" {
			return fmt.Errorf("request %s is %s, not Pending", reqID, cur.Status)
		}
		grant = store.GitRepoGrant{
			ID:             NewID("grepo-grant"),
			AgentNamespace: req.AgentNamespace,
			AgentName:      req.AgentName,
			ServiceAccount: "claw-agent-" + req.AgentName,
			ImageDigest:    agent.Status.SelectedImageDigest,
			AgentSpecHash:  agent.Status.AgentSpecHash,
			GitRepoID:      req.GitRepoID,
			Access:         req.Access,
			ApprovedBy:     approver,
			Reason:         reason,
		}
		if e := tx.CreateGitRepoGrant(grant); e != nil {
			return e
		}
		if e := tx.SetGitRepoRequestStatus(reqID, "Approved"); e != nil {
			return e
		}
		return tx.AppendAudit(store.AuditEvent{
			Type: "gitrepo.request.approved", GrantID: grant.ID, Actor: approver,
			Detail: map[string]any{"request": reqID, "repo": req.RepoName, "access": req.Access, "reason": reason},
		})
	})
	if err != nil {
		return store.GitRepoGrant{}, err
	}
	s.resume(ctx, req, grant)
	return grant, nil
}

// Deny marks a request Denied.
func (s *Service) Deny(ctx context.Context, reqID, actor, reason string) error {
	return s.Store.Tx(ctx, func(tx store.Tx) error {
		req, err := tx.GetGitRepoRequest(reqID)
		if err != nil {
			return err
		}
		if err := tx.SetGitRepoRequestStatus(reqID, "Denied"); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{
			Type: "gitrepo.request.denied", Actor: actor,
			Detail: map[string]any{"request": reqID, "repo": req.RepoName, "reason": reason},
		})
	})
}

// RevokeGrant revokes a git-repo grant and audits it.
func (s *Service) RevokeGrant(ctx context.Context, grantID, actor, reason string) error {
	return s.Store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.RevokeGitRepoGrant(grantID, reason); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{
			Type: "gitrepo.grant.revoked", GrantID: grantID, Actor: actor,
			Detail: map[string]any{"reason": reason},
		})
	})
}

// resume auto-continues the run that asked for access, mirroring the on-demand
// secret flow: it enqueues a follow-up turn in the original session so the agent
// retries (retrieve-first now finds the grant) without a user nudge.
func (s *Service) resume(ctx context.Context, req store.GitRepoRequest, grant store.GitRepoGrant) {
	if req.RunID == "" {
		return
	}
	_ = s.Store.Tx(ctx, func(tx store.Tx) error {
		run, e := tx.GetRun(req.RunID)
		if e != nil || run.SessionID == "" {
			return nil
		}
		cont := store.Run{
			ID:             NewID("run"),
			AgentNamespace: run.AgentNamespace,
			AgentName:      run.AgentName,
			SessionID:      run.SessionID,
			Phase:          "Pending",
			Source:         run.Source,
			Input:          `{"text":"The git repo access I requested was just approved — please continue with my previous request."}`,
		}
		if e := tx.CreateRun(cont); e != nil {
			return e
		}
		return tx.AppendAudit(store.AuditEvent{Type: "gitrepo.access_resumed", RunID: cont.ID,
			GrantID: grant.ID, Detail: map[string]any{"originalRun": req.RunID}})
	})
}

func (s *Service) isGranter(ctx context.Context, req store.GitRepoRequest, principal string) (bool, error) {
	var found bool
	err := s.Store.Tx(ctx, func(tx store.Tx) error {
		repo, e := tx.GetGitRepoByID(req.GitRepoID)
		if e != nil {
			return e
		}
		for _, g := range repo.Granters {
			if g == principal {
				found = true
				return nil
			}
		}
		return nil
	})
	return found, err
}
