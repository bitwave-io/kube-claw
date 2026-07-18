package models

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

func testService(t *testing.T) *Service {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dir, "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := secrets.NewLocalCipher(filepath.Join(dir, "master.keyset"))
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Store: st, Cipher: cipher}
}

func TestModelServiceResolution(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)

	// Empty registry resolves nowhere (runner falls back to env).
	if _, err := svc.Resolve(ctx, "thread1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty registry resolve = %v, want ErrNotFound", err)
	}

	// Register two models; keys encrypt at rest and decrypt on resolve.
	if err := svc.Upsert(ctx, store.Model{Name: "opus", Provider: "anthropic", ModelID: "claude-opus-4-8"}, "sk-ant-secret"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Upsert(ctx, store.Model{Name: "local", Provider: "openai", ModelID: "llama-4",
		BaseURL: "http://vllm.internal/v1"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetDefault(ctx, "opus"); err != nil {
		t.Fatal(err)
	}

	// Ciphertext in the store must not be the plaintext.
	list, _ := svc.List(ctx)
	for _, m := range list {
		if strings.Contains(string(m.APIKeyCiphertext), "sk-ant-secret") {
			t.Fatal("api key stored unencrypted")
		}
	}

	// Default resolution decrypts the key.
	r, err := svc.Resolve(ctx, "thread1")
	if err != nil || r.Name != "opus" || r.APIKey != "sk-ant-secret" || r.Source != "default" {
		t.Fatalf("default resolve = %+v, %v", r, err)
	}

	// Session override wins; keyless self-hosted resolves with empty key.
	if err := svc.SetSessionModel(ctx, "thread1", "local", "run-1"); err != nil {
		t.Fatal(err)
	}
	r, err = svc.Resolve(ctx, "thread1")
	if err != nil || r.Name != "local" || r.Source != "session" || r.APIKey != "" || r.BaseURL != "http://vllm.internal/v1" {
		t.Fatalf("session resolve = %+v, %v", r, err)
	}
	// Other sessions still get the default.
	if r, _ = svc.Resolve(ctx, "thread2"); r.Name != "opus" {
		t.Fatalf("other-session resolve = %+v", r)
	}

	// Switching to an unregistered model is refused.
	if err := svc.SetSessionModel(ctx, "thread1", "nope", "run-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("switch to unknown = %v, want ErrNotFound", err)
	}

	// The default can't be deleted while another model exists.
	if err := svc.Delete(ctx, "opus"); err == nil || !strings.Contains(err.Error(), "default") {
		t.Fatalf("deleting the default must be refused, got %v", err)
	}

	// Validation: bad provider, spacey name.
	if err := svc.Upsert(ctx, store.Model{Name: "x", Provider: "gemini", ModelID: "y"}, ""); err == nil {
		t.Fatal("unknown provider must be rejected")
	}
	if err := svc.Upsert(ctx, store.Model{Name: "two words", Provider: "openai", ModelID: "y"}, ""); err == nil {
		t.Fatal("spacey name must be rejected")
	}
}
