// Package connector is the generic external-connector plane: any message
// source (a SaaS product, a gateway service, a web-chat backend) registers a
// callback URL and gets an ingest URL + API key. Inbound messages become runs
// exactly like Slack messages do; run outputs are pushed back to the callback
// URL as signed webhooks (webhook.go).
//
// The Slack connector (internal/router/slack) predates this package and keeps
// its own transport; both converge on the same store.Run model.
package connector

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/traego/kube-claw/internal/store"
)

// NewAPIKey mints a connector API key and its storable hash. The key is shown
// once at creation/rotation; only the hash is persisted.
func NewAPIKey() (key, hash string) {
	key = "ck_" + randHex(24)
	return key, HashKey(key)
}

// HashKey returns the stored form of an API key: hex(sha256(key)).
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// NewSigningSecret mints the HMAC secret used to sign outbound callbacks.
func NewSigningSecret() string { return "cs_" + randHex(24) }

// NewConnectorID mints a connector id.
func NewConnectorID() string { return "conn-" + randHex(8) }

// SessionKey namespaces a connector-supplied session id under the connector,
// so a session can never collide with (or claim the history and warm pod of)
// a Slack thread or another connector's session.
func SessionKey(connectorID, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return connectorID + ":" + sessionID
}

// ExternalSessionID recovers the connector's original session id from a stored
// (namespaced) one, for delivery back to the callback.
func ExternalSessionID(connectorID, sessionKey string) string {
	return strings.TrimPrefix(sessionKey, connectorID+":")
}

// Message is one inbound connector message.
type Message struct {
	// EventID dedupes redelivered messages (empty = no dedupe).
	EventID string `json:"eventId"`
	// SessionID is the connector's conversation key (a thread, a chat, a
	// ticket). Messages sharing it share agent, history, and the warm pod.
	SessionID string `json:"sessionId"`
	Text      string `json:"text"`
	User      string `json:"user"`
}

// source is the run Source JSON for connector-triggered runs.
type source struct {
	Trigger   string `json:"trigger"` // always "webhook"
	Connector string `json:"connector"`
	Event     string `json:"event,omitempty"`
	User      string `json:"user,omitempty"`
}

// SourceConnectorID returns the connector id from a run's Source JSON, or ""
// if the run was not connector-triggered.
func SourceConnectorID(src string) string {
	var s source
	if json.Unmarshal([]byte(src), &s) != nil || s.Trigger != "webhook" {
		return ""
	}
	return s.Connector
}

// Ingest dedupes an inbound message and creates a run for it. A message that
// continues an existing session reuses that session's agent (mirroring Slack
// thread replies); a new session gets the connector's pinned agent. Returns
// the new run id, or "" if the event was a duplicate.
func Ingest(ctx context.Context, st store.Store, conn store.Connector, msg Message) (string, error) {
	agentNS, agentName := conn.AgentNamespace, conn.AgentName
	sessionKey := SessionKey(conn.ID, msg.SessionID)
	if sessionKey != "" {
		_ = st.Tx(ctx, func(tx store.Tx) error {
			if runs, e := tx.ListRunsBySession(sessionKey, 1); e == nil && len(runs) > 0 {
				agentNS, agentName = runs[0].AgentNamespace, runs[0].AgentName
			}
			return nil
		})
	}
	src, err := json.Marshal(source{Trigger: "webhook", Connector: conn.ID, Event: msg.EventID, User: msg.User})
	if err != nil {
		return "", err
	}
	input, err := json.Marshal(map[string]string{"text": msg.Text})
	if err != nil {
		return "", err
	}
	runID := "run-" + randHex(8)
	created := false
	err = st.Tx(ctx, func(tx store.Tx) error {
		if msg.EventID != "" {
			dup, err := tx.SeenEvent("connector:"+conn.ID, msg.EventID)
			if err != nil || dup {
				return err
			}
		}
		if err := tx.CreateRun(store.Run{
			ID: runID, AgentNamespace: agentNS, AgentName: agentName,
			SessionID: sessionKey, Phase: "Pending",
			Source: string(src), Input: string(input),
		}); err != nil {
			return err
		}
		created = true
		return tx.AppendAudit(store.AuditEvent{Type: "connector.event_received", RunID: runID, Actor: conn.ID,
			Detail: map[string]any{"connector": conn.Name}})
	})
	if err != nil || !created {
		return "", err
	}
	return runID, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("connector: rand: %v", err))
	}
	return hex.EncodeToString(b)
}
