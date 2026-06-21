package secrets

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	st, err := sqlite.Open(context.Background(), filepath.Join(dir, "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, err := NewLocalCipher(filepath.Join(dir, "master.keyset"))
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Store: st, Cipher: c}
}

func TestService_CreatePutGet(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	if _, err := svc.CreateSecret(ctx, "claw-agents", "gcp-billing", "gcp.serviceAccountKey", "", []string{"U123"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	secretVal := []byte(`{"private_key":"xyz"}`)
	if err := svc.PutValue(ctx, "claw-agents", "gcp-billing", secretVal, "tester"); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := svc.GetValue(ctx, "claw-agents", "gcp-billing")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(secretVal) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestService_IntakeSingleUse(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	if _, err := svc.CreateSecret(ctx, "claw-agents", "slack-token", "slack", "", nil); err != nil {
		t.Fatal(err)
	}

	tok, err := svc.MintIntakeToken(ctx, "claw-agents", "slack-token")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := svc.SubmitIntake(ctx, tok, []byte("xoxb-secret")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// value stored
	got, err := svc.GetValue(ctx, "claw-agents", "slack-token")
	if err != nil || string(got) != "xoxb-secret" {
		t.Fatalf("value after intake = %q err=%v", got, err)
	}
	// second submit with the same token must fail (single-use)
	if err := svc.SubmitIntake(ctx, tok, []byte("attacker")); !errors.Is(err, store.ErrTokenUsed) {
		t.Fatalf("reuse err = %v, want ErrTokenUsed", err)
	}
	// bogus token → not found
	if err := svc.SubmitIntake(ctx, "deadbeef", []byte("x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bogus token err = %v, want ErrNotFound", err)
	}
}
