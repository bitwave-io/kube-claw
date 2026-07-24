package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/traego/kube-claw/internal/store"
)

func TestProviderRegistry(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	err = st.Tx(ctx, func(tx store.Tx) error {
		if err := tx.UpsertProvider(store.Provider{Name: "openai-prod", Kind: "openai",
			Enabled: true, ModelPrefix: "oai-", APIKeyCiphertext: []byte("ct")}); err != nil {
			return err
		}
		p, err := tx.GetProvider("openai-prod")
		if err != nil || p.Kind != "openai" || string(p.APIKeyCiphertext) != "ct" || !p.Enabled {
			t.Fatalf("get provider = %+v, %v", p, err)
		}

		// Empty ciphertext on update keeps the stored key.
		if err := tx.UpsertProvider(store.Provider{Name: "openai-prod", Kind: "openai", Enabled: false}); err != nil {
			return err
		}
		if p, _ := tx.GetProvider("openai-prod"); string(p.APIKeyCiphertext) != "ct" || p.Enabled {
			t.Fatalf("update must keep key + apply enabled=false: %+v", p)
		}

		// Sync state round-trips.
		if err := tx.SetProviderSyncState("openai-prod", "2026-07-23T00:00:00Z", "boom"); err != nil {
			return err
		}
		if p, _ := tx.GetProvider("openai-prod"); p.LastSyncError != "boom" || p.LastSyncedAt == "" {
			t.Fatalf("sync state not recorded: %+v", p)
		}

		// A discovered model + its session pin are dropped when the provider goes.
		if err := tx.UpsertModel(store.Model{Name: "oai-gpt5", Provider: "openai", ModelID: "gpt-5.2",
			ProviderName: "openai-prod", Enabled: true}); err != nil {
			return err
		}
		if err := tx.SetSessionModel("thread1", "oai-gpt5", "run-1"); err != nil {
			return err
		}
		if err := tx.DeleteProvider("openai-prod"); err != nil {
			return err
		}
		if _, err := tx.GetProvider("openai-prod"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("provider not deleted: %v", err)
		}
		if _, err := tx.GetModel("oai-gpt5"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("discovered model must be deleted with its provider: %v", err)
		}
		if _, err := tx.GetSessionModel("thread1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("session pin to a deleted model must be gone: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSetModelEnabled(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	err = st.Tx(ctx, func(tx store.Tx) error {
		if err := tx.UpsertModel(store.Model{Name: "m", Provider: "openai", ModelID: "x", Enabled: true}); err != nil {
			return err
		}
		if err := tx.SetModelEnabled("m", false); err != nil {
			return err
		}
		if m, _ := tx.GetModel("m"); m.Enabled {
			t.Fatal("SetModelEnabled(false) did not stick")
		}
		if err := tx.SetModelEnabled("nope", true); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("SetModelEnabled on unknown = %v, want ErrNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
