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

	// Register two models; keys encrypt at rest and decrypt on resolve. The
	// first registered model becomes the default inside the same transaction.
	if err := svc.Upsert(ctx, store.Model{Name: "opus", Provider: "anthropic", ModelID: "claude-opus-4-8"}, "sk-ant-secret", false); err != nil {
		t.Fatal(err)
	}
	if r, err := svc.Resolve(ctx, "thread1"); err != nil || r.Name != "opus" {
		t.Fatalf("first model must become the default: %+v, %v", r, err)
	}
	if err := svc.Upsert(ctx, store.Model{Name: "local", Provider: "openai", ModelID: "llama-4",
		BaseURL: "http://vllm.internal/v1", MaxTokens: 8192}, "", false); err != nil {
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
	if err != nil || r.Name != "local" || r.Source != "session" || r.APIKey != "" ||
		r.BaseURL != "http://vllm.internal/v1" || r.MaxTokens != 8192 {
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

	// Explicit default=true moves the default (same transaction as the upsert).
	if err := svc.Upsert(ctx, store.Model{Name: "local", Provider: "openai", ModelID: "llama-4",
		BaseURL: "http://vllm.internal/v1"}, "", true); err != nil {
		t.Fatal(err)
	}
	if r, _ := svc.Resolve(ctx, "thread2"); r.Name != "local" {
		t.Fatalf("default must have moved to local, got %+v", r)
	}

	// Validation: bad provider, spacey name, negative cap.
	if err := svc.Upsert(ctx, store.Model{Name: "x", Provider: "gemini", ModelID: "y"}, "", false); err == nil {
		t.Fatal("unknown provider must be rejected")
	}
	if err := svc.Upsert(ctx, store.Model{Name: "two words", Provider: "openai", ModelID: "y"}, "", false); err == nil {
		t.Fatal("spacey name must be rejected")
	}
	if err := svc.Upsert(ctx, store.Model{Name: "x", Provider: "openai", ModelID: "y", MaxTokens: -1}, "", false); err == nil {
		t.Fatal("negative maxTokens must be rejected")
	}
}

// stubCatalog returns a fixed model list, ignoring the provider config.
type stubCatalog struct{ ids []DiscoveredModel }

func (c stubCatalog) List(ctx context.Context, p store2Provider) ([]DiscoveredModel, error) {
	return c.ids, nil
}

func TestSyncProviderAndResolve(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)
	svc.Catalog = stubCatalog{ids: []DiscoveredModel{
		{ModelID: "gpt-5.2", WireFormat: "openai", InheritsKey: true},
		{ModelID: "gpt-5-mini", WireFormat: "openai", InheritsKey: true},
	}}

	if err := svc.UpsertProvider(ctx, store.Provider{Name: "openai-prod", Kind: "openai", Enabled: true, ModelPrefix: "oai-"}, "sk-provider"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SyncProvider(ctx, "openai-prod"); err != nil {
		t.Fatal(err)
	}

	// Both discovered models land, enabled, prefixed, with no own key.
	list, _ := svc.List(ctx)
	if len(list) != 2 {
		t.Fatalf("want 2 discovered models, got %d: %+v", len(list), list)
	}
	byName := map[string]store.Model{}
	for _, m := range list {
		byName[m.Name] = m
		if !m.Enabled {
			t.Fatalf("discovered model %q should be enabled", m.Name)
		}
		if m.ProviderName != "openai-prod" || len(m.APIKeyCiphertext) != 0 {
			t.Fatalf("discovered model %q should link to provider and hold no own key: %+v", m.Name, m)
		}
	}
	if _, ok := byName["oai-gpt-5.2"]; !ok {
		t.Fatalf("prefix not applied: %v", byName)
	}

	// Resolve inherits the provider's key (the first model is auto-default).
	r, err := svc.Resolve(ctx, "")
	if err != nil || r.APIKey != "sk-provider" {
		t.Fatalf("resolve must inherit provider key: %+v, %v", r, err)
	}

	// A manual disable must survive a resync.
	if err := svc.SetModelEnabled(ctx, "oai-gpt-5-mini", false); err != nil {
		t.Fatal(err)
	}
	if err := svc.SyncProvider(ctx, "openai-prod"); err != nil {
		t.Fatal(err)
	}
	after, _ := svc.List(ctx)
	if modelByName(after, "oai-gpt-5-mini").Enabled {
		t.Fatal("manual disable must survive resync")
	}

	// A disabled model is not in the enabled list and can't be switched to.
	enabled, _ := svc.ListEnabled(ctx)
	for _, m := range enabled {
		if m.Name == "oai-gpt-5-mini" {
			t.Fatal("disabled model must not appear in ListEnabled")
		}
	}
	if err := svc.SetSessionModel(ctx, "thread1", "oai-gpt-5-mini", "run-1"); err == nil {
		t.Fatal("switching to a disabled model must be refused")
	}

	// Disabling the default is refused (a session must always resolve).
	def, _ := svc.Resolve(ctx, "")
	if err := svc.SetModelEnabled(ctx, def.Name, false); err == nil {
		t.Fatal("disabling the default must be refused")
	}

	// Deleting a provider removes its models; refused while it owns the default.
	if err := svc.DeleteProvider(ctx, "openai-prod"); err == nil {
		t.Fatal("deleting a provider that owns the default must be refused")
	}
	// Re-point the default to a manual model, then the provider deletes cleanly.
	if err := svc.Upsert(ctx, store.Model{Name: "manual", Provider: "anthropic", ModelID: "claude-opus-4-8"}, "sk", true); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteProvider(ctx, "openai-prod"); err != nil {
		t.Fatalf("provider delete after re-pointing default: %v", err)
	}
	left, _ := svc.List(ctx)
	if len(left) != 1 || left[0].Name != "manual" {
		t.Fatalf("provider delete must drop its models, left: %+v", left)
	}
}

// TestSyncProviderHandleCollision verifies a catalog sync never overwrites a
// handle it doesn't own — neither a hand-entered model nor one from a different
// provider. Clobbering would silently repoint someone else's model at this
// provider's endpoint and key.
func TestSyncProviderHandleCollision(t *testing.T) {
	ctx := context.Background()
	svc := testService(t)

	// A hand-entered model named exactly what the provider's sync will produce.
	if err := svc.Upsert(ctx, store.Model{Name: "gpt-5.2", Provider: "openai", ModelID: "manual-pin", BaseURL: "https://manual.example/v1"}, "sk-manual", true); err != nil {
		t.Fatal(err)
	}

	// Provider with NO prefix, so its discovered "gpt-5.2" collides with the manual one.
	svc.Catalog = stubCatalog{ids: []DiscoveredModel{
		{ModelID: "gpt-5.2", WireFormat: "openai", InheritsKey: true},
		{ModelID: "gpt-5-mini", WireFormat: "openai", InheritsKey: true},
	}}
	if err := svc.UpsertProvider(ctx, store.Provider{Name: "openai-prod", Kind: "openai", Enabled: true}, "sk-provider"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SyncProvider(ctx, "openai-prod"); err != nil {
		t.Fatal(err)
	}

	list, _ := svc.List(ctx)
	m := modelByName(list, "gpt-5.2")
	// The manual model must be untouched: still manual (no ProviderName), own key,
	// own endpoint and model id.
	if m.ProviderName != "" || m.ModelID != "manual-pin" || m.BaseURL != "https://manual.example/v1" || len(m.APIKeyCiphertext) == 0 {
		t.Fatalf("sync overwrote a colliding manual model: %+v", m)
	}
	// The non-colliding discovered model still lands.
	if got := modelByName(list, "gpt-5-mini"); got.ProviderName != "openai-prod" {
		t.Fatalf("non-colliding discovered model should land: %+v", got)
	}
}

func modelByName(list []store.Model, name string) store.Model {
	for _, m := range list {
		if m.Name == name {
			return m
		}
	}
	return store.Model{}
}
