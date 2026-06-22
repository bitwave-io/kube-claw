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
