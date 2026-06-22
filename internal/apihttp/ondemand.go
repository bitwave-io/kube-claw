package apihttp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	slackrouter "github.com/traego/kube-claw/internal/router/slack"
	"github.com/traego/kube-claw/internal/store"
)

// requestSecretReq is what the agent's request_secret tool sends.
type requestSecretReq struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// requestSecret lets a running agent ask for a credential it discovered it needs.
// It creates the secret (granter = the Slack user who started the run), mints a
// one-time intake link, and DMs that user. Self-service: the user providing the
// value via the link IS the authorization (they're the granter).
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
	var run store.Run
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetRun(runID)
		run = got
		return e
	}); err != nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	user := slackrouter.SlackUser(run.Source)
	if user == "" {
		writeErr(w, http.StatusBadRequest, "no Slack user on this run to ask")
		return
	}
	// Create (idempotent) with the requesting user as granter, then mint a link.
	_, _ = s.Secrets.CreateSecret(r.Context(), run.AgentNamespace, req.Name, "", req.Description, []string{user})
	tok, err := s.Secrets.MintIntakeToken(r.Context(), run.AgentNamespace, req.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "mint intake: "+err.Error())
		return
	}
	if s.Notifier != nil {
		link := fmt.Sprintf("%s/ui/secret-intake/%s", s.UIBase, tok)
		msg := fmt.Sprintf("An agent needs the secret *%s* to answer your request", req.Name)
		if req.Description != "" {
			msg += fmt.Sprintf(" (%s)", req.Description)
		}
		msg += ".\nAdd it with this one-time link (you're the approver):\n" + link
		_ = s.Notifier.PostReply(r.Context(), user, "", msg)
	}
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		return tx.AppendAudit(store.AuditEvent{Type: "secret.requested_ondemand", RunID: runID,
			Detail: map[string]any{"secret": req.Name, "user": user}})
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "requested", "user": user})
}

// requestedSecret returns an on-demand secret's value to the run once the user
// has provided it. Authorized by the run's session token + the requesting user
// being the secret's granter (self-service). 204 while not yet provided.
func (s *Server) requestedSecret(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if claims, err := s.Signer.Verify(bearer(r)); err != nil || claims.RunID != runID {
		writeErr(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
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
	user := slackrouter.SlackUser(run.Source)

	// Authorize: the requesting user must be a granter of this secret (self-service).
	var ok bool
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		sec, e := tx.GetSecret(run.AgentNamespace, name)
		if e != nil {
			return nil
		}
		for _, g := range sec.Granters {
			if g == user {
				ok = true
			}
		}
		return nil
	})
	if !ok {
		writeErr(w, http.StatusForbidden, "not authorized for this secret")
		return
	}

	val, err := s.Secrets.GetValue(r.Context(), run.AgentNamespace, name)
	if err != nil || len(val) == 0 {
		w.WriteHeader(http.StatusNoContent) // not provided yet — caller polls
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
