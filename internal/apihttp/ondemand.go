package apihttp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/approvals"
	slackrouter "github.com/traego/kube-claw/internal/router/slack"
	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
)

// sessionHistory returns the prior succeeded turns (request + answer) for a run's
// session, so a cold-start pod can replay the conversation. A run may only read
// its own session's history (token-scoped).
func (s *Server) sessionHistory(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	claims, err := s.Signer.Verify(bearer(r))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	type turn struct {
		Input  string `json:"input"`
		Output string `json:"output"`
	}
	turns := []turn{}
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		caller, e := tx.GetRun(claims.RunID)
		if e != nil || caller.SessionID != sid {
			return nil // caller not in this session → empty history
		}
		runs, e := tx.ListRunsBySession(sid, 50)
		if e != nil {
			return e
		}
		for _, run := range runs {
			if run.ID == claims.RunID || run.Phase != "Succeeded" {
				continue
			}
			in := jsonField(run.Input, "text")
			out := ""
			if outs, e := tx.ListOutputs(run.ID); e == nil && len(outs) > 0 {
				out = outs[len(outs)-1].Content
			}
			// Output may legitimately be empty: coalesced/declined runs complete
			// with a "none" output, but their INPUT is still part of the
			// conversation — dropping it would lose instructions and corrections
			// across a pod restart. Emit them as user-only turns; the runner
			// folds them into the adjacent answered turn on replay.
			if in != "" {
				turns = append(turns, turn{Input: in, Output: out})
			}
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]any{"turns": turns})
}

// availableSecrets lists the secret names + descriptions (never values) in the
// run's namespace, so the agent knows what credentials it can request/retrieve by
// name instead of guessing. Token-scoped to the run.
func (s *Server) availableSecrets(w http.ResponseWriter, r *http.Request) {
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
	type sec struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	out := []sec{}
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		all, e := tx.ListSecrets()
		if e != nil {
			return e
		}
		for _, x := range all {
			if x.Namespace == run.AgentNamespace {
				out = append(out, sec{Name: x.Name, Description: x.Description})
			}
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]any{"secrets": out})
}

// requestSecretReq is what the agent's request_secret tool sends.
type requestSecretReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Reason      string `json:"reason"` // the agent's justification ("why"), shown to approvers
}

// validSecretName allows only safe characters so a name can't be used for path
// traversal when it's interpolated into the secret file path.
func validSecretName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return name != "." && name != ".." && !strings.Contains(name, "..")
}

// agentBinding loads an agent's current grant binding (digest + spec hash).
func (s *Server) agentBinding(ctx context.Context, ns, name string) (digest, specHash string) {
	var a clawv1alpha1.Agent
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &a); err == nil {
		return a.Status.SelectedImageDigest, a.Status.AgentSpecHash
	}
	return "", ""
}

// requestSecret handles an agent's on-demand credential request:
//   - secret doesn't exist  → provision it (requester becomes granter, agent is
//     self-granted) and DM the requester a one-time intake link.
//   - exists + agent granted → no-op (the agent retrieves it next poll).
//   - exists + not granted  → open a SecretRequest and post Approve/Deny to the
//     secret's GRANTERS with the agent's reason + who it's for (PAM access flow).
func (s *Server) requestSecret(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if claims, err := s.Signer.Verify(bearer(r)); err != nil || claims.RunID != runID {
		writeErr(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	var req requestSecretReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if !validSecretName(req.Name) {
		writeErr(w, http.StatusBadRequest, "invalid secret name")
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

	var sec store.Secret
	exists := false
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if got, e := tx.GetSecret(ns, req.Name); e == nil {
			sec, exists = got, true
		}
		return nil
	})

	// 1. Doesn't exist → provision (self-service) + self-grant the agent.
	if !exists {
		if user == "" {
			writeErr(w, http.StatusBadRequest, "no Slack user on this run to ask")
			return
		}
		if _, err := s.Secrets.CreateSecret(r.Context(), ns, req.Name, "", req.Description, []string{user}); err != nil {
			writeErr(w, http.StatusInternalServerError, "create secret: "+err.Error())
			return
		}
		_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
			got, e := tx.GetSecret(ns, req.Name)
			if e != nil {
				return e
			}
			grant := store.Grant{
				ID: secrets.NewID("grant"), AgentNamespace: ns, AgentName: agentName,
				ServiceAccount: "claw-agent-" + agentName, ImageDigest: digest, AgentSpecHash: specHash,
				DeliveryHash: approvals.OnDemandDeliveryHash, SecretID: got.ID,
				ApprovedBy: user, Reason: "self-service provision",
			}
			if e := tx.CreateGrant(grant); e != nil {
				return e
			}
			return tx.AppendAudit(store.AuditEvent{Type: "secret.provisioned", RunID: runID, SecretID: got.ID,
				Actor: user, Detail: map[string]any{"secret": req.Name}})
		})
		tok, err := s.Secrets.MintIntakeToken(r.Context(), ns, req.Name, runID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "mint intake: "+err.Error())
			return
		}
		if s.Notifier != nil {
			link := fmt.Sprintf("%s/ui/secret-intake/%s", s.UIBase, tok)
			msg := fmt.Sprintf("An agent needs *%s* to answer your request", req.Name)
			if req.Description != "" {
				msg += fmt.Sprintf(" (%s)", req.Description)
			}
			msg += ".\nAdd it with this one-time link:\n" + link
			_ = s.Notifier.PostReply(r.Context(), user, "", msg)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "provisioning"})
		return
	}

	// 2. Exists + agent already granted → nothing to do.
	granted := false
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if _, e := tx.FindValidGrant(ns, agentName, sec.ID, digest, specHash, approvals.OnDemandDeliveryHash); e == nil {
			granted = true
		}
		return nil
	})
	if granted {
		writeJSON(w, http.StatusOK, map[string]string{"status": "granted"})
		return
	}

	// 3. Exists + not granted → request access from the secret's granters.
	var reqID string
	isNew := false
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if existing, e := tx.GetPendingRequest(ns, agentName, sec.ID); e == nil {
			reqID = existing.ID
			return nil
		}
		reqID = secrets.NewID("req")
		if e := tx.CreateSecretRequest(store.SecretRequest{
			ID: reqID, Status: "Pending", AgentNamespace: ns, AgentName: agentName, RunID: runID,
			SecretID: sec.ID, SecretName: req.Name, ImageDigest: digest,
			Context: req.Reason, RequestedBy: user,
		}); e != nil {
			return e
		}
		isNew = true
		return tx.AppendAudit(store.AuditEvent{Type: "secret.access_requested", RunID: runID, SecretID: sec.ID,
			Actor: agentName, Detail: map[string]any{"secret": req.Name, "requestedBy": user, "reason": req.Reason}})
	})
	if isNew && s.Notifier != nil {
		for _, g := range sec.Granters {
			_ = s.Notifier.PostAccessRequest(r.Context(), g, req.Name, agentName, user, req.Reason, reqID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "access_requested"})
}

// requestedSecret returns an on-demand secret's value once the agent holds a valid
// grant (granter-approved or self-provisioned) and the value is present.
func (s *Server) requestedSecret(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if claims, err := s.Signer.Verify(bearer(r)); err != nil || claims.RunID != runID {
		writeErr(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" || !validSecretName(name) {
		writeErr(w, http.StatusBadRequest, "invalid or missing secret name")
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

	granted := false
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		sec, e := tx.GetSecret(ns, name)
		if e != nil {
			return nil
		}
		if _, ge := tx.FindValidGrant(ns, agentName, sec.ID, digest, specHash, approvals.OnDemandDeliveryHash); ge == nil {
			granted = true
		}
		return nil
	})
	if !granted {
		w.WriteHeader(http.StatusNoContent) // not granted yet — caller polls
		return
	}
	val, err := s.Secrets.GetValue(r.Context(), ns, name)
	if err != nil || len(val) == 0 {
		w.WriteHeader(http.StatusNoContent) // granted but value not provided yet
		return
	}
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		return tx.AppendAudit(store.AuditEvent{Type: "secret.materialized", RunID: runID,
			Detail: map[string]any{"secret": name, "ondemand": true}})
	})
	writeJSON(w, http.StatusOK, map[string]string{
		"name":    name,
		"path":    "/var/run/claw/secrets/" + name + ".json",
		"content": base64.StdEncoding.EncodeToString(val),
	})
}
