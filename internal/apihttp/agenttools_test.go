package apihttp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
