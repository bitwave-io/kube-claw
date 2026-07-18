package slack

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

func TestParseRegisterSecret(t *testing.T) {
	for _, tc := range []struct{ in, name, desc string }{
		{"register secret gcp-billing read-only billing key", "gcp-billing", "read-only billing key"},
		{"register secret slack-token", "slack-token", ""},
		{"I want to register a secret", "", ""}, // "secret" is last word → no name
		{"hello", "", ""},
	} {
		n, d := parseRegisterSecret(tc.in)
		if n != tc.name || d != tc.desc {
			t.Errorf("parseRegisterSecret(%q) = (%q,%q), want (%q,%q)", tc.in, n, d, tc.name, tc.desc)
		}
	}
}

func TestHandleDM_RegistersSecretWithDMUserAsGranter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dir, "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := secrets.NewLocalCipher(filepath.Join(dir, "master.keyset"))
	r := &Router{Secrets: &secrets.Service{Store: st, Cipher: cipher}, UIBase: "http://ui"}

	// Non-register DM → usage hint.
	if reply := r.HandleDM(ctx, "U_ALEX", "hi there"); !strings.Contains(reply, "register secret") {
		t.Fatalf("usage hint = %q", reply)
	}

	// Register → one-time link + the DMing user becomes the granter.
	reply := r.HandleDM(ctx, "U_ALEX", "register secret gcp-billing read-only billing key")
	if !strings.Contains(reply, "http://ui/ui/secret-intake/") {
		t.Fatalf("reply missing intake link: %q", reply)
	}
	_ = st.Tx(ctx, func(tx store.Tx) error {
		sec, e := tx.GetSecret("claw-agents", "gcp-billing")
		if e != nil {
			t.Fatalf("secret not created: %v", e)
		}
		if len(sec.Granters) != 1 || sec.Granters[0] != "U_ALEX" {
			t.Fatalf("granters = %v, want [U_ALEX]", sec.Granters)
		}
		if sec.Description != "read-only billing key" {
			t.Fatalf("description = %q", sec.Description)
		}
		return nil
	})
}

// DM "announce releases in #channel" sets the management channel; only the
// upgrade admin may change it once one is claimed; "stop" clears it.
func TestAnnounceReleasesDM(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	r := &Router{Store: st}

	getMgmt := func() string {
		var v string
		_ = st.Tx(ctx, func(tx store.Tx) error {
			v, _ = tx.GetSetting(store.SettingMgmtChannel)
			return nil
		})
		return v
	}

	// No admin claimed: anyone can set it.
	reply := r.HandleDM(ctx, "U1", "announce releases in <#C0MGMT|kube-claw-mgmt>")
	if !strings.Contains(reply, "<#C0MGMT>") || getMgmt() != "C0MGMT" {
		t.Fatalf("set failed: reply=%q mgmt=%q", reply, getMgmt())
	}

	// Missing channel ref → usage hint, unchanged.
	reply = r.HandleDM(ctx, "U1", "announce releases somewhere")
	if !strings.Contains(reply, "Which channel") || getMgmt() != "C0MGMT" {
		t.Fatalf("usage hint failed: reply=%q mgmt=%q", reply, getMgmt())
	}

	// With an admin claimed, others are refused.
	if err := st.Tx(ctx, func(tx store.Tx) error {
		return tx.SetSetting(store.SettingUpgradeAdmin, "U_ADMIN")
	}); err != nil {
		t.Fatal(err)
	}
	reply = r.HandleDM(ctx, "U_OTHER", "announce releases in <#C0EVIL>")
	if !strings.Contains(reply, "upgrade admin") || getMgmt() != "C0MGMT" {
		t.Fatalf("non-admin change not refused: reply=%q mgmt=%q", reply, getMgmt())
	}

	// The admin can change and stop it.
	_ = r.HandleDM(ctx, "U_ADMIN", "announce releases in <#C1NEW>")
	if getMgmt() != "C1NEW" {
		t.Fatalf("admin change failed: mgmt=%q", getMgmt())
	}
	reply = r.HandleDM(ctx, "U_ADMIN", "stop announcing releases")
	if !strings.Contains(reply, "stop") || getMgmt() != "" {
		t.Fatalf("stop failed: reply=%q mgmt=%q", reply, getMgmt())
	}
}

// A DM that matches no deterministic command becomes an agent run on installs
// with an LLM (Classifier) configured — the DM channel is the session and the
// run is marked dm — while the exact commands stay string-matched. LLM-less
// installs keep the usage reply.
func TestConversationalDM(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// LLM-less: chatter gets the usage reply, never "".
	bare := &Router{Store: st}
	if reply := bare.HandleDM(ctx, "U1", "what clusters do we run?"); !strings.Contains(reply, "register secret") {
		t.Fatalf("LLM-less DM reply = %q, want usage text", reply)
	}

	// With a Classifier, chatter falls through to conversation…
	r := &Router{Store: st, DefaultAgent: "general", Classifier: NewClassifier("test-key")}
	if reply := r.HandleDM(ctx, "U1", "what clusters do we run?"); reply != "" {
		t.Fatalf("conversational DM reply = %q, want \"\" (fall through to a run)", reply)
	}
	// …and the commands stay deterministic.
	if reply := r.HandleDM(ctx, "U1", "announce releases in <#C0MGMT>"); !strings.Contains(reply, "<#C0MGMT>") {
		t.Fatalf("command hijacked by conversation: %q", reply)
	}

	// The conversation run: session = DM channel, dm-marked, deduped.
	runID, err := r.HandleDMMessage(ctx, "1111.0001", "D0PAT", "<@U1>: what clusters do we run?", "U1")
	if err != nil || runID == "" {
		t.Fatalf("HandleDMMessage: run=%q err=%v", runID, err)
	}
	runs, err := listSession(ctx, st, "D0PAT")
	if err != nil || len(runs) != 1 {
		t.Fatalf("session runs = %d err=%v", len(runs), err)
	}
	if !strings.Contains(runs[0].Source, `"dm":true`) || !strings.Contains(runs[0].Source, `"user":"U1"`) {
		t.Fatalf("run source = %s", runs[0].Source)
	}
	if runs[0].AgentName != "general" {
		t.Fatalf("agent = %q, want the default", runs[0].AgentName)
	}
	// Duplicate event → no second run.
	if dup, err := r.HandleDMMessage(ctx, "1111.0001", "D0PAT", "same again", "U1"); err != nil || dup != "" {
		t.Fatalf("dedupe failed: run=%q err=%v", dup, err)
	}
	// No default agent → no run.
	noAgent := &Router{Store: st, Classifier: NewClassifier("test-key")}
	if id, _ := noAgent.HandleDMMessage(ctx, "1111.0002", "D0PAT", "hi", "U1"); id != "" {
		t.Fatalf("run without any agent = %q", id)
	}
}

// DM "check for updates" requests an immediate release check via the upgrade
// actor — deterministically, even on conversational installs.
func TestCheckForUpdatesDM(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	fake := &fakeUpgrades{}
	r := &Router{Store: st, Upgrades: fake, Classifier: NewClassifier("test-key")}

	if reply := r.HandleDM(ctx, "U1", "can you check for updates?"); !strings.Contains(reply, "checking") || fake.checks != 1 {
		t.Fatalf("check command: reply=%q checks=%d", reply, fake.checks)
	}
	// Without the actor, a clear message instead of a run.
	bare := &Router{Store: st, Classifier: NewClassifier("test-key")}
	if reply := bare.HandleDM(ctx, "U1", "check for a new release please"); !strings.Contains(reply, "isn't enabled") {
		t.Fatalf("actor-less reply = %q", reply)
	}
}
