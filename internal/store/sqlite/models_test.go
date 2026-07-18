package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/traego/kube-claw/internal/store"
)

func TestModelRegistry(t *testing.T) {
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
		// Upsert + get round-trip.
		if err := tx.UpsertModel(store.Model{Name: "opus", Provider: "anthropic", ModelID: "claude-opus-4-8",
			APIKeyCiphertext: []byte("ct1")}); err != nil {
			return err
		}
		if err := tx.UpsertModel(store.Model{Name: "gpt5", Provider: "openai", ModelID: "gpt-5.2",
			BaseURL: "https://api.openai.com/v1", APIKeyCiphertext: []byte("ct2")}); err != nil {
			return err
		}
		m, err := tx.GetModel("gpt5")
		if err != nil || m.ModelID != "gpt-5.2" || string(m.APIKeyCiphertext) != "ct2" {
			t.Fatalf("get gpt5 = %+v, %v", m, err)
		}

		// Update with EMPTY ciphertext keeps the stored key; new ciphertext replaces.
		if err := tx.UpsertModel(store.Model{Name: "gpt5", Provider: "openai", ModelID: "gpt-5.3"}); err != nil {
			return err
		}
		m, _ = tx.GetModel("gpt5")
		if m.ModelID != "gpt-5.3" || string(m.APIKeyCiphertext) != "ct2" {
			t.Fatalf("blank-key update must keep the key: %+v", m)
		}
		if err := tx.UpsertModel(store.Model{Name: "gpt5", Provider: "openai", ModelID: "gpt-5.3",
			APIKeyCiphertext: []byte("ct3")}); err != nil {
			return err
		}
		if m, _ = tx.GetModel("gpt5"); string(m.APIKeyCiphertext) != "ct3" {
			t.Fatalf("new key must replace: %+v", m)
		}

		// Default semantics: none → ErrNotFound; set moves atomically.
		if _, err := tx.GetDefaultModel(); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("default before set = %v, want ErrNotFound", err)
		}
		if err := tx.SetDefaultModel("opus"); err != nil {
			return err
		}
		if err := tx.SetDefaultModel("gpt5"); err != nil {
			return err
		}
		d, err := tx.GetDefaultModel()
		if err != nil || d.Name != "gpt5" {
			t.Fatalf("default = %+v, %v", d, err)
		}
		if m, _ := tx.GetModel("opus"); m.IsDefault {
			t.Fatal("old default must be cleared")
		}
		if err := tx.SetDefaultModel("nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("default to unknown = %v, want ErrNotFound", err)
		}

		// List: default first.
		list, err := tx.ListModels()
		if err != nil || len(list) != 2 || !list[0].IsDefault {
			t.Fatalf("list = %+v, %v", list, err)
		}

		// Session pin + delete cascade.
		if err := tx.SetSessionModel("thread1", "opus", "run-1"); err != nil {
			return err
		}
		if n, err := tx.GetSessionModel("thread1"); err != nil || n != "opus" {
			t.Fatalf("session model = %q, %v", n, err)
		}
		if err := tx.DeleteModel("opus"); err != nil {
			return err
		}
		if _, err := tx.GetSessionModel("thread1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("deleting a model must drop its session pins")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
