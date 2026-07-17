package apihttp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/connector"
	"github.com/traego/kube-claw/internal/store"
)

// Connector-plane endpoints: an external message source (a SaaS integration, a
// gateway service) registers a callback URL and gets back an ingest URL + API
// key. Management endpoints share the admin basic-auth credential; the ingest
// endpoint authenticates with the connector's own API key, so it is safe to
// expose on the public URL.

// adminOK authorizes connector-management calls with the same basic-auth
// credential as /ui (user "admin"). No admin password configured (local/dev)
// = open, matching the rest of the /v1 surface.
func (s *Server) adminOK(r *http.Request) bool {
	if s.AdminPassword == "" {
		return true
	}
	u, p, ok := r.BasicAuth()
	return ok && u == "admin" && subtle.ConstantTimeCompare([]byte(p), []byte(s.AdminPassword)) == 1
}

type createConnectorReq struct {
	Name        string `json:"name"`
	CallbackURL string `json:"callbackUrl"`
	Agent       struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	} `json:"agent"`
}

// createConnector registers a connector and mints its credentials. The API key
// and signing secret are returned ONCE and are not retrievable afterwards.
func (s *Server) createConnector(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeErr(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	var req createConnectorReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || req.Agent.Name == "" {
		writeErr(w, http.StatusBadRequest, "name and agent.name are required")
		return
	}
	if !strings.HasPrefix(req.CallbackURL, "http://") && !strings.HasPrefix(req.CallbackURL, "https://") {
		writeErr(w, http.StatusBadRequest, "callbackUrl must be an http(s) URL")
		return
	}
	if req.Agent.Namespace == "" {
		req.Agent.Namespace = "claw-agents"
	}
	// The pinned agent must exist — a typo here would otherwise surface as
	// every ingested run spinning forever on a missing Agent CR.
	var agent clawv1alpha1.Agent
	if err := s.Reader.Get(r.Context(), client.ObjectKey{Namespace: req.Agent.Namespace, Name: req.Agent.Name}, &agent); err != nil {
		writeErr(w, http.StatusBadRequest, "agent not found: "+req.Agent.Namespace+"/"+req.Agent.Name)
		return
	}
	key, hash := connector.NewAPIKey()
	conn := store.Connector{
		ID:             connector.NewConnectorID(),
		Name:           req.Name,
		CallbackURL:    req.CallbackURL,
		APIKeyHash:     hash,
		SigningSecret:  connector.NewSigningSecret(),
		AgentNamespace: req.Agent.Namespace,
		AgentName:      req.Agent.Name,
	}
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if err := tx.CreateConnector(conn); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "connector.created", Actor: "admin",
			Detail: map[string]any{"connector": conn.ID, "name": conn.Name, "agent": conn.AgentNamespace + "/" + conn.AgentName}})
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"id":            conn.ID,
		"name":          conn.Name,
		"ingestPath":    "/v1/connectors/" + conn.ID + "/messages",
		"apiKey":        key,
		"signingSecret": conn.SigningSecret,
	})
}

func (s *Server) listConnectors(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeErr(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	var conns []store.Connector
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListConnectors()
		conns = got
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if conns == nil {
		conns = []store.Connector{}
	}
	writeJSON(w, http.StatusOK, conns) // hash + signing secret are json:"-"
}

func (s *Server) deleteConnector(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeErr(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	id := r.PathValue("id")
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if err := tx.DeleteConnector(id); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "connector.deleted", Actor: "admin",
			Detail: map[string]any{"connector": id}})
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// rotateConnectorKey mints a new API key (returned once); the old key stops
// working in the same transaction.
func (s *Server) rotateConnectorKey(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeErr(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	id := r.PathValue("id")
	key, hash := connector.NewAPIKey()
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if err := tx.SetConnectorKeyHash(id, hash); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "connector.key_rotated", Actor: "admin",
			Detail: map[string]any{"connector": id}})
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "connector not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "apiKey": key})
}

// connectorIngest is the inbound message endpoint: Bearer <apiKey> → a run.
// The key alone identifies the connector; the path id must match it so a
// leaked URL can't be replayed against another connector's key.
func (s *Server) connectorIngest(w http.ResponseWriter, r *http.Request) {
	key := bearer(r)
	if key == "" {
		writeErr(w, http.StatusUnauthorized, "missing API key")
		return
	}
	var conn store.Connector
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetConnectorByKeyHash(connector.HashKey(key))
		conn = got
		return e
	})
	if errors.Is(err, store.ErrNotFound) || (err == nil && conn.ID != r.PathValue("id")) {
		writeErr(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if conn.Disabled {
		writeErr(w, http.StatusForbidden, "connector is disabled")
		return
	}
	var msg connector.Message
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil || strings.TrimSpace(msg.Text) == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	runID, err := connector.Ingest(r.Context(), s.Store, conn, msg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if runID == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"runId": runID, "sessionId": msg.SessionID})
}

// deliverToConnector pushes a run event to the owning connector's callback URL.
// Delivery (with retries) runs in the background so a slow or down receiver
// never blocks the runner's output callback; outputs are already persisted and
// queryable via /v1/runs either way.
func (s *Server) deliverToConnector(connID string, run store.Run, kind, content string) {
	var conn store.Connector
	if err := s.Store.Tx(context.Background(), func(tx store.Tx) error {
		got, e := tx.GetConnector(connID)
		conn = got
		return e
	}); err != nil {
		logf.Log.WithName("apihttp").Error(err, "connector lookup for delivery", "connector", connID, "run", run.ID)
		return
	}
	ev := connector.Event{
		RunID:     run.ID,
		SessionID: connector.ExternalSessionID(connID, run.SessionID),
		Kind:      kind,
		Content:   content,
	}
	deliverer := s.Deliverer
	if deliverer == nil {
		deliverer = &connector.Deliverer{}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		info := connector.ConnectorInfo{ID: conn.ID, CallbackURL: conn.CallbackURL, SigningSecret: conn.SigningSecret}
		if err := deliverer.Deliver(ctx, info, ev); err != nil {
			logf.Log.WithName("apihttp").Error(err, "connector delivery failed", "connector", connID, "run", run.ID)
			_ = s.Store.Tx(context.Background(), func(tx store.Tx) error {
				return tx.AppendAudit(store.AuditEvent{Type: "connector.delivery_failed", RunID: run.ID, Actor: connID,
					Detail: map[string]any{"error": err.Error()}})
			})
		}
	}()
}
