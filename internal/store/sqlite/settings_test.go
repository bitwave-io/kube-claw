package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/traego/kube-claw/internal/store"
)

// TestSettings exercises the install-wide KV: set/get round-trip, replace,
// ErrNotFound, and the first-claim-wins semantics backing the upgrade admin
// (DESIGN.md §24.6).
func TestSettings(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if err := s.Tx(ctx, func(tx store.Tx) error {
		if _, err := tx.GetSetting(store.SettingUpgradeAdmin); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetSetting(unset) err = %v, want ErrNotFound", err)
		}

		// First claim wins; second claim is a no-op.
		set, err := tx.SetSettingIfUnset(store.SettingUpgradeAdmin, "U_FIRST")
		if err != nil || !set {
			t.Fatalf("first SetSettingIfUnset = (%v, %v), want (true, nil)", set, err)
		}
		set, err = tx.SetSettingIfUnset(store.SettingUpgradeAdmin, "U_SECOND")
		if err != nil || set {
			t.Fatalf("second SetSettingIfUnset = (%v, %v), want (false, nil)", set, err)
		}
		if v, err := tx.GetSetting(store.SettingUpgradeAdmin); err != nil || v != "U_FIRST" {
			t.Errorf("GetSetting = (%q, %v), want (U_FIRST, nil)", v, err)
		}

		// SetSetting replaces unconditionally (the explicit override path).
		if err := tx.SetSetting(store.SettingUpgradeAdmin, "U_OVERRIDE"); err != nil {
			t.Fatalf("SetSetting: %v", err)
		}
		if v, _ := tx.GetSetting(store.SettingUpgradeAdmin); v != "U_OVERRIDE" {
			t.Errorf("after override GetSetting = %q, want U_OVERRIDE", v)
		}
		return nil
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}
}
