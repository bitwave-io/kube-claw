// Package approvals is the single approval path shared by the API/CLI
// (break-glass) and the Slack connector (granter-checked). Both compute the
// grant binding from the agent's CURRENT state (DESIGN.md §8, §16).
package approvals

import (
	"context"
	"errors"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
)

// ErrNotGranter is returned when a principal is not authorized to approve.
var ErrNotGranter = errors.New("principal is not a granter for this secret")

type Service struct {
	Store   store.Store
	Secrets *secrets.Service
	Reader  client.Reader
}

// Approve is the break-glass path (no granter check; caller is already trusted,
// e.g. the authenticated CLI/API).
func (s *Service) Approve(ctx context.Context, reqID, approver, reason string) (store.Grant, error) {
	req, binding, err := s.load(ctx, reqID)
	if err != nil {
		return store.Grant{}, err
	}
	grant, err := s.Secrets.ApproveRequest(ctx, reqID, approver, reason, binding)
	if err == nil {
		s.resumeOnDemand(ctx, req, grant)
	}
	return grant, err
}

// ApproveByPrincipal is the Slack path: the principal (Slack user id) MUST be a
// configured granter of the secret (DESIGN.md §8.1).
func (s *Service) ApproveByPrincipal(ctx context.Context, reqID, principal, reason string) (store.Grant, error) {
	req, binding, err := s.load(ctx, reqID)
	if err != nil {
		return store.Grant{}, err
	}
	ok, err := s.isGranter(ctx, req, principal)
	if err != nil {
		return store.Grant{}, err
	}
	if !ok {
		return store.Grant{}, ErrNotGranter
	}
	grant, err := s.Secrets.ApproveRequest(ctx, reqID, principal, reason, binding)
	if err == nil {
		s.resumeOnDemand(ctx, req, grant)
	}
	return grant, err
}

// resumeOnDemand auto-continues an on-demand request after approval: it enqueues
// a follow-up turn in the original Slack thread so the agent retries (retrieve-
// first now finds the grant and installs it) without the user nudging it. Only
// for on-demand grants (declared-secret runs resume via the run engine's gate).
func (s *Service) resumeOnDemand(ctx context.Context, req store.SecretRequest, grant store.Grant) {
	if grant.DeliveryHash != OnDemandDeliveryHash || req.RunID == "" {
		return
	}
	_ = s.Store.Tx(ctx, func(tx store.Tx) error {
		run, e := tx.GetRun(req.RunID)
		if e != nil || run.SessionID == "" {
			return nil // no session to resume into
		}
		cont := store.Run{
			ID:             secrets.NewID("run"),
			AgentNamespace: run.AgentNamespace,
			AgentName:      run.AgentName,
			SessionID:      run.SessionID,
			Phase:          "Pending",
			Source:         run.Source, // same channel/thread/user
			Input:          `{"text":"The credential I requested was just approved — please continue with my previous request."}`,
		}
		if e := tx.CreateRun(cont); e != nil {
			return e
		}
		return tx.AppendAudit(store.AuditEvent{Type: "secret.access_resumed", RunID: cont.ID,
			SecretID: req.SecretID, GrantID: grant.ID, Detail: map[string]any{"originalRun": req.RunID}})
	})
}

// Deny denies a request.
func (s *Service) Deny(ctx context.Context, reqID, actor, reason string) error {
	return s.Secrets.DenyRequest(ctx, reqID, actor, reason)
}

func (s *Service) isGranter(ctx context.Context, req store.SecretRequest, principal string) (bool, error) {
	var found bool
	err := s.Store.Tx(ctx, func(tx store.Tx) error {
		sec, e := tx.GetSecret(req.AgentNamespace, req.SecretName)
		if e != nil {
			return e
		}
		for _, g := range sec.Granters {
			if g == principal {
				found = true
				return nil
			}
		}
		return nil
	})
	return found, err
}

// load fetches the request and computes the grant binding from the live Agent.
func (s *Service) load(ctx context.Context, reqID string) (store.SecretRequest, secrets.GrantBinding, error) {
	var req store.SecretRequest
	if err := s.Store.Tx(ctx, func(tx store.Tx) error {
		got, e := tx.GetSecretRequest(reqID)
		req = got
		return e
	}); err != nil {
		return req, secrets.GrantBinding{}, err
	}

	var agent clawv1alpha1.Agent
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: req.AgentNamespace, Name: req.AgentName}, &agent); err != nil {
		return req, secrets.GrantBinding{}, fmt.Errorf("load agent: %w", err)
	}
	for _, sref := range agent.Spec.Secrets {
		if sref.Name == req.SecretName {
			return req, secrets.GrantBinding{
				ImageDigest:    agent.Status.SelectedImageDigest,
				AgentSpecHash:  agent.Status.AgentSpecHash,
				DeliveryHash:   secrets.DeliveryHash(sref.Delivery.Path, sref.Delivery.Mode, sref.Delivery.Env),
				ServiceAccount: "claw-agent-" + agent.Name,
			}, nil
		}
	}
	// On-demand secret (not declared on the agent): bind to the agent's current
	// digest+spec with a fixed on-demand delivery hash (the agent chooses the path).
	return req, secrets.GrantBinding{
		ImageDigest:    agent.Status.SelectedImageDigest,
		AgentSpecHash:  agent.Status.AgentSpecHash,
		DeliveryHash:   OnDemandDeliveryHash,
		ServiceAccount: "claw-agent-" + agent.Name,
	}, nil
}

// OnDemandDeliveryHash is the fixed delivery binding for grants created via the
// request_secret flow (the agent picks the file path at runtime, so the grant
// authorizes the agent for the secret rather than a specific delivery path).
const OnDemandDeliveryHash = "ondemand"
