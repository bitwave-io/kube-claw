package apihttp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/traego/kube-claw/internal/approvals"
	"github.com/traego/kube-claw/internal/store"
)

// TestAgentRegisterSecret: the register_secret runner callback creates the
// secret with the run's Slack user as granter and mints a one-time intake
// link; non-Slack runs are refused.
func TestAgentRegisterSecret(t *testing.T) {
	s := fullServer(t)
	h := s.handler()
	ctx := context.Background()

	if err := s.Store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.CreateRun(store.Run{ID: "run-dm", AgentNamespace: "claw-agents", AgentName: "general",
			SessionID: "D0IM", Phase: "Running",
			Source: `{"trigger":"slack","channel":"D0IM","event":"1.2","user":"U_PAT","dm":true}`}); err != nil {
			return err
		}
		return tx.CreateRun(store.Run{ID: "run-cli", AgentNamespace: "claw-agents", AgentName: "general",
			SessionID: "", Phase: "Running", Source: `{"trigger":"cli"}`})
	}); err != nil {
		t.Fatal(err)
	}
	dmTok, _ := s.Signer.Issue("run-dm", nil, time.Hour)
	cliTok, _ := s.Signer.Issue("run-cli", nil, time.Hour)

	if rr := do(t, h, "POST", "/v1/runs/run-dm/register-secret", `{"name":"gh-token"}`); rr.Code != 401 {
		t.Fatalf("unauthenticated = %d", rr.Code)
	}
	if rr := doAuth(t, h, "POST", "/v1/runs/run-cli/register-secret", `{"name":"gh-token"}`, cliTok); rr.Code != 403 {
		t.Fatalf("non-slack run = %d, want 403", rr.Code)
	}
	rr := doAuth(t, h, "POST", "/v1/runs/run-dm/register-secret", `{"name":"gh-token","description":"deploy token"}`, dmTok)
	if rr.Code != 201 {
		t.Fatalf("register = %d (%s)", rr.Code, rr.Body)
	}
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["granter"] != "U_PAT" || !strings.HasPrefix(out["url"], "http://ui/ui/secret-intake/") {
		t.Fatalf("register response = %v", out)
	}
	// The secret exists with the DMing user as granter.
	var granters []string
	if err := s.Store.Tx(ctx, func(tx store.Tx) error {
		sec, e := tx.GetSecret("claw-agents", "gh-token")
		if e != nil {
			return e
		}
		granters = sec.Granters
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(granters) != 1 || granters[0] != "U_PAT" {
		t.Fatalf("granters = %v", granters)
	}
}

// TestAgentSetAnnounceChannel: admin gating and validation for the
// set_release_announce_channel runner callback.
func TestAgentSetAnnounceChannel(t *testing.T) {
	s := fullServer(t)
	h := s.handler()
	ctx := context.Background()

	if err := s.Store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.CreateRun(store.Run{ID: "run-pat", AgentNamespace: "claw-agents", AgentName: "general",
			SessionID: "D0IM", Phase: "Running",
			Source: `{"trigger":"slack","channel":"D0IM","event":"1.2","user":"U_PAT","dm":true}`}); err != nil {
			return err
		}
		return tx.CreateRun(store.Run{ID: "run-other", AgentNamespace: "claw-agents", AgentName: "general",
			SessionID: "th-9", Phase: "Running",
			Source: `{"trigger":"slack","channel":"C9","event":"9.9","user":"U_OTHER"}`})
	}); err != nil {
		t.Fatal(err)
	}
	patTok, _ := s.Signer.Issue("run-pat", nil, time.Hour)
	otherTok, _ := s.Signer.Issue("run-other", nil, time.Hour)

	getMgmt := func() string {
		var v string
		_ = s.Store.Tx(ctx, func(tx store.Tx) error { v, _ = tx.GetSetting(store.SettingMgmtChannel); return nil })
		return v
	}

	// Garbage channel id → 400.
	if rr := doAuth(t, h, "POST", "/v1/runs/run-pat/announce-channel", `{"channel":"ops"}`, patTok); rr.Code != 400 {
		t.Fatalf("bad channel id = %d", rr.Code)
	}
	// No admin claimed: anyone may set it.
	if rr := doAuth(t, h, "POST", "/v1/runs/run-pat/announce-channel", `{"channel":"C0MGMT"}`, patTok); rr.Code != 200 || getMgmt() != "C0MGMT" {
		t.Fatalf("set = %d mgmt=%q", rr.Code, getMgmt())
	}
	// Admin claimed: only the admin may change it.
	if err := s.Store.Tx(ctx, func(tx store.Tx) error {
		return tx.SetSetting(store.SettingUpgradeAdmin, "U_PAT")
	}); err != nil {
		t.Fatal(err)
	}
	if rr := doAuth(t, h, "POST", "/v1/runs/run-other/announce-channel", `{"channel":"C0EVIL"}`, otherTok); rr.Code != 403 || getMgmt() != "C0MGMT" {
		t.Fatalf("non-admin = %d mgmt=%q, want 403 + unchanged", rr.Code, getMgmt())
	}
	// The admin can clear it.
	if rr := doAuth(t, h, "POST", "/v1/runs/run-pat/announce-channel", `{"channel":""}`, patTok); rr.Code != 200 || getMgmt() != "" {
		t.Fatalf("clear = %d mgmt=%q", rr.Code, getMgmt())
	}
}

// TestRequestSecretGrantedButValueless: a secret that was self-service
// provisioned but never filled must NOT be a black hole. A later request from
// a different user re-mints an intake link for THAT user (audited), instead of
// answering "granted" and pinging nobody. Once a value exists, the same
// request really is a "granted" no-op.
func TestRequestSecretGrantedButValueless(t *testing.T) {
	s := fullServer(t)
	h := s.handler()
	ctx := context.Background()

	// U_ALICE provisioned kraken-ro on an earlier run (granter + self-grant),
	// but never used her intake link — the secret has no value.
	if _, err := s.Secrets.CreateSecret(ctx, "claw-agents", "kraken-ro", "", "read-only key", []string{"U_ALICE"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Tx(ctx, func(tx store.Tx) error {
		sec, e := tx.GetSecret("claw-agents", "kraken-ro")
		if e != nil {
			return e
		}
		if e := tx.CreateGrant(store.Grant{
			ID: "grant-old", AgentNamespace: "claw-agents", AgentName: "general",
			DeliveryHash: approvals.OnDemandDeliveryHash, SecretID: sec.ID, ApprovedBy: "U_ALICE",
		}); e != nil {
			return e
		}
		return tx.CreateRun(store.Run{ID: "run-bob", AgentNamespace: "claw-agents", AgentName: "general",
			SessionID: "th-1", Phase: "Running",
			Source: `{"trigger":"slack","channel":"C1","event":"1.1","user":"U_BOB"}`})
	}); err != nil {
		t.Fatal(err)
	}
	bobTok, _ := s.Signer.Issue("run-bob", nil, time.Hour)

	// U_BOB asks for the same secret → intake is re-run for Bob, not "granted".
	rr := doAuth(t, h, "POST", "/v1/runs/run-bob/request-secret", `{"name":"kraken-ro"}`, bobTok)
	if rr.Code != 200 {
		t.Fatalf("request = %d (%s)", rr.Code, rr.Body)
	}
	var out struct {
		Status   string   `json:"status"`
		Notified []string `json:"notified"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Status != "provisioning" {
		t.Fatalf("status = %q, want provisioning (valueless grant must re-run intake)", out.Status)
	}
	if len(out.Notified) != 0 { // no Notifier wired in tests → nobody was DM'd, and the response must say so
		t.Fatalf("notified = %v, want empty without a Notifier", out.Notified)
	}
	var audits []store.AuditRecord
	if err := s.Store.Tx(ctx, func(tx store.Tx) error {
		got, e := tx.ListAudit(50)
		audits = got
		return e
	}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audits {
		if a.Type == "secret.intake_reissued" && a.Actor == "U_BOB" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no secret.intake_reissued audit for U_BOB in %v", audits)
	}

	// With a value present the request is a plain "granted" no-op again.
	if err := s.Secrets.PutValue(ctx, "claw-agents", "kraken-ro", []byte(`{"k":"v"}`), "U_ALICE"); err != nil {
		t.Fatal(err)
	}
	rr = doAuth(t, h, "POST", "/v1/runs/run-bob/request-secret", `{"name":"kraken-ro"}`, bobTok)
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if rr.Code != 200 || out.Status != "granted" {
		t.Fatalf("with value: %d %q, want 200 granted", rr.Code, out.Status)
	}
}

// TestRequestSecretPendingRequestReported: when an access request is already
// pending, a repeat request must say so (pending=true + who the granters are)
// rather than pretending a fresh ping went out.
func TestRequestSecretPendingRequestReported(t *testing.T) {
	s := fullServer(t)
	h := s.handler()
	ctx := context.Background()

	// Secret exists (granter U_GRANTER, no grant for this agent, value present).
	if _, err := s.Secrets.CreateSecret(ctx, "claw-agents", "gh-pat", "", "", []string{"U_GRANTER"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Secrets.PutValue(ctx, "claw-agents", "gh-pat", []byte("x"), "U_GRANTER"); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Tx(ctx, func(tx store.Tx) error {
		return tx.CreateRun(store.Run{ID: "run-bob", AgentNamespace: "claw-agents", AgentName: "general",
			SessionID: "th-1", Phase: "Running",
			Source: `{"trigger":"slack","channel":"C1","event":"1.1","user":"U_BOB"}`})
	}); err != nil {
		t.Fatal(err)
	}
	bobTok, _ := s.Signer.Issue("run-bob", nil, time.Hour)

	var out struct {
		Status   string   `json:"status"`
		Granters []string `json:"granters"`
		Pending  bool     `json:"pending"`
	}
	rr := doAuth(t, h, "POST", "/v1/runs/run-bob/request-secret", `{"name":"gh-pat","reason":"ship it"}`, bobTok)
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if rr.Code != 200 || out.Status != "access_requested" || out.Pending {
		t.Fatalf("first request: %d %+v, want access_requested pending=false", rr.Code, out)
	}
	rr = doAuth(t, h, "POST", "/v1/runs/run-bob/request-secret", `{"name":"gh-pat","reason":"ship it"}`, bobTok)
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if rr.Code != 200 || out.Status != "access_requested" || !out.Pending {
		t.Fatalf("repeat request: %d %+v, want pending=true", rr.Code, out)
	}
	if len(out.Granters) != 1 || out.Granters[0] != "U_GRANTER" {
		t.Fatalf("granters = %v, want [U_GRANTER]", out.Granters)
	}
}
