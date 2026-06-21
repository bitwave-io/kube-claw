// Package apihttp serves the controller's HTTP API (DESIGN.md §13).
//
// Phase 2 testability slice: enough to smoke-test a deployed controller —
//
//	GET  /healthz        liveness
//	GET  /v1/agents      list Agents (proves the k8s client + reconciler)
//	POST /v1/runs        create a run (proves the SQLite write path + audit)
//	GET  /v1/runs/{id}   read it back (proves persistence on the PVC)
//
// Auth (SA-token / claw session token) and TLS are layered in later phases; for
// now this is a cluster-internal, port-forwarded surface.
package apihttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/identity"
	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
)

// Server is a controller-runtime Runnable that serves the HTTP API.
type Server struct {
	Addr     string
	Store    store.Store
	Reader   client.Reader     // uncached k8s reader (mgr.GetAPIReader)
	Secrets  *secrets.Service  // secret authority
	UIBase   string            // base URL of the intake UI (for returned links)
	Identity identity.Provider // /login credential verifier
	Signer   *identity.Signer  // claw session token signer
}

// NeedLeaderElection lets the API run on every replica (false = not gated).
func (s *Server) NeedLeaderElection() bool { return false }

// Start runs the HTTP server until ctx is cancelled (manager.Runnable).
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()
	logf.Log.WithName("apihttp").Info("serving HTTP API", "addr", s.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/agents", s.listAgents)
	mux.HandleFunc("POST /v1/runs", s.createRun)
	mux.HandleFunc("GET /v1/runs/{id}", s.getRun)
	mux.HandleFunc("POST /v1/runs/{id}/outputs", s.postOutput)
	mux.HandleFunc("POST /v1/secrets", s.createSecret)
	mux.HandleFunc("GET /v1/secrets/{name}/metadata", s.secretMetadata)
	mux.HandleFunc("PUT /v1/secrets/{name}/versions", s.putSecretVersion)
	mux.HandleFunc("GET /v1/secret-requests", s.listRequests)
	mux.HandleFunc("POST /v1/secret-requests/{id}/approve", s.approveRequest)
	mux.HandleFunc("POST /v1/secret-requests/{id}/deny", s.denyRequest)
	mux.HandleFunc("GET /v1/secret-grants", s.listGrants)
	mux.HandleFunc("POST /v1/secret-grants/{id}/revoke", s.revokeGrant)
	mux.HandleFunc("POST /v1/login", s.login)
	mux.HandleFunc("POST /v1/runs/{id}/materialize", s.materialize)
	return mux
}

type agentView struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Digest    string `json:"digest"`
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	var list clawv1alpha1.AgentList
	if err := s.Reader.List(r.Context(), &list); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]agentView, 0, len(list.Items))
	for _, a := range list.Items {
		out = append(out, agentView{a.Namespace, a.Name, a.Status.Phase, a.Status.SelectedImageDigest})
	}
	writeJSON(w, http.StatusOK, out)
}

type createRunReq struct {
	Namespace string `json:"namespace"`
	Agent     string `json:"agent"`
	Input     string `json:"input"`
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var req createRunReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Agent == "" || req.Namespace == "" {
		writeErr(w, http.StatusBadRequest, "namespace and agent are required")
		return
	}
	run := store.Run{
		ID:             newRunID(),
		AgentNamespace: req.Namespace,
		AgentName:      req.Agent,
		Phase:          "Pending",
		Input:          mustJSON(map[string]string{"text": req.Input}),
		Source:         mustJSON(map[string]string{"trigger": "cli"}),
		CreatedAt:      store.NowRFC3339(),
	}
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if err := tx.CreateRun(run); err != nil {
			return err
		}
		// Audit in the same tx (the store invariant).
		return tx.AppendAudit(store.AuditEvent{
			Type:  "agentrun.created",
			RunID: run.ID,
			Actor: "cli",
			Detail: map[string]any{
				"agent":     req.Agent,
				"namespace": req.Namespace,
			},
		})
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": run.ID, "phase": run.Phase})
}

type runView struct {
	store.Run
	Outputs []store.Output `json:"outputs"`
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var view runView
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		run, e := tx.GetRun(id)
		if e != nil {
			return e
		}
		outs, e := tx.ListOutputs(id)
		if e != nil {
			return e
		}
		view = runView{Run: run, Outputs: outs}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if view.Outputs == nil {
		view.Outputs = []store.Output{}
	}
	writeJSON(w, http.StatusOK, view)
}

type postOutputReq struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// postOutput records a runner's output and marks the run Succeeded. This is the
// runner→controller callback (DESIGN.md §36). Auth (claw session token) lands in
// Phase 5; for now it is unauthenticated on the cluster-internal API.
func (s *Server) postOutput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req postOutputReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Kind == "" {
		req.Kind = "text"
	}
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if _, e := tx.GetRun(id); e != nil {
			return e
		}
		if e := tx.AppendOutput(id, store.Output{Kind: req.Kind, Content: req.Content}); e != nil {
			return e
		}
		if e := tx.MarkRunSucceeded(id); e != nil {
			return e
		}
		return tx.AppendAudit(store.AuditEvent{Type: "agentrun.completed", RunID: id, Actor: "runner"})
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

// --- secrets (Phase 3) ---

type createSecretReq struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Granters  []string `json:"granters"`
}

// createSecret records metadata + granters and mints a one-time intake link.
func (s *Server) createSecret(w http.ResponseWriter, r *http.Request) {
	var req createSecretReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Namespace == "" || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "namespace and name are required")
		return
	}
	sec, err := s.Secrets.CreateSecret(r.Context(), req.Namespace, req.Name, req.Type, req.Granters)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tok, err := s.Secrets.MintIntakeToken(r.Context(), req.Namespace, req.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	path := "/ui/secret-intake/" + tok
	writeJSON(w, http.StatusCreated, map[string]string{
		"id":          sec.ID,
		"intakePath":  path,
		"intakeURL":   s.UIBase + path,
	})
}

func (s *Server) secretMetadata(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	name := r.PathValue("name")
	var sec store.Secret
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetSecret(ns, name)
		sec = got
		return e
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "secret not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sec) // metadata only — never the value
}

// putSecretVersion is the break-glass / CI value upload (DESIGN.md §8.3).
func (s *Server) putSecretVersion(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	name := r.PathValue("name")
	value, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read body")
		return
	}
	if len(value) == 0 {
		writeErr(w, http.StatusBadRequest, "empty value")
		return
	}
	if err := s.Secrets.PutValue(r.Context(), ns, name, value, "cli"); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "secret not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stored"})
}

// --- approval (Phase 4) ---

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	var reqs []store.SecretRequest
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListSecretRequests(status)
		reqs = got
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if reqs == nil {
		reqs = []store.SecretRequest{}
	}
	writeJSON(w, http.StatusOK, reqs)
}

type approveReq struct {
	Approver string `json:"approver"`
	Reason   string `json:"reason"`
}

func (s *Server) approveRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body approveReq
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Approver == "" {
		body.Approver = "cli"
	}

	// Load the request, then the live Agent, to bind the grant to current state.
	var req store.SecretRequest
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetSecretRequest(id)
		req = got
		return e
	}); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "request not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	binding, err := s.bindingFor(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	grant, err := s.Secrets.ApproveRequest(r.Context(), id, body.Approver, body.Reason, binding)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"grant": grant.ID, "status": "approved"})
}

// bindingFor computes the grant binding from the agent's CURRENT state for the
// secret named in the request (DESIGN.md §8 — approve what is current).
func (s *Server) bindingFor(ctx context.Context, req store.SecretRequest) (secrets.GrantBinding, error) {
	var agent clawv1alpha1.Agent
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: req.AgentNamespace, Name: req.AgentName}, &agent); err != nil {
		return secrets.GrantBinding{}, fmt.Errorf("load agent: %w", err)
	}
	for _, sref := range agent.Spec.Secrets {
		if sref.Name == req.SecretName {
			return secrets.GrantBinding{
				ImageDigest:    agent.Status.SelectedImageDigest,
				AgentSpecHash:  agent.Status.AgentSpecHash,
				DeliveryHash:   secrets.DeliveryHash(sref.Delivery.Path, sref.Delivery.Mode, sref.Delivery.Env),
				ServiceAccount: "claw-agent-" + agent.Name,
			}, nil
		}
	}
	return secrets.GrantBinding{}, fmt.Errorf("agent %s no longer references secret %q", req.AgentName, req.SecretName)
}

func (s *Server) denyRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body approveReq
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.Secrets.DenyRequest(r.Context(), id, body.Approver, body.Reason); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "request not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "denied"})
}

func (s *Server) listGrants(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	agent := r.URL.Query().Get("agent")
	var grants []store.Grant
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListGrants(ns, agent)
		grants = got
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if grants == nil {
		grants = []store.Grant{}
	}
	writeJSON(w, http.StatusOK, grants)
}

func (s *Server) revokeGrant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body approveReq
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.Secrets.RevokeGrant(r.Context(), id, body.Approver, body.Reason); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "grant not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// --- helpers ---

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "run-" + hex.EncodeToString(b)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
