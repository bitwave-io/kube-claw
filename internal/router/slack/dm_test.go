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
