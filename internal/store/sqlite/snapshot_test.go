package sqlite

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSnapshotBeforeMigrate: a version change snapshots the DB before
// migration; the same version doesn't; old snapshots are pruned.
func TestSnapshotBeforeMigrate(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "claw.db")
	if err := os.WriteFile(db, []byte("v1-data"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Fresh marker state (first boot on this version) → snapshot taken.
	snap, err := SnapshotBeforeMigrate(dir, "claw.db", "v0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if snap != db+".pre-v0.4.0" {
		t.Fatalf("snapshot path = %q", snap)
	}
	if data, _ := os.ReadFile(snap); string(data) != "v1-data" {
		t.Fatalf("snapshot content = %q", data)
	}
	if err := WriteVersionMarker(dir, "v0.4.0"); err != nil {
		t.Fatal(err)
	}

	// Same version again (ordinary restart) → no snapshot.
	if snap, err = SnapshotBeforeMigrate(dir, "claw.db", "v0.4.0"); err != nil || snap != "" {
		t.Fatalf("same-version snapshot = (%q, %v), want none", snap, err)
	}

	// New versions snapshot again; pruning keeps the newest two.
	for _, v := range []string{"v0.5.0", "v0.6.0", "v0.7.0"} {
		if _, err := SnapshotBeforeMigrate(dir, "claw.db", v); err != nil {
			t.Fatal(err)
		}
		if err := WriteVersionMarker(dir, v); err != nil {
			t.Fatal(err)
		}
	}
	matches, _ := filepath.Glob(db + ".pre-*")
	if len(matches) != 2 {
		t.Fatalf("kept %d snapshots (%v), want 2", len(matches), matches)
	}

	// No DB file (fresh install) → no snapshot, no error.
	fresh := t.TempDir()
	if snap, err = SnapshotBeforeMigrate(fresh, "claw.db", "v0.4.0"); err != nil || snap != "" {
		t.Fatalf("fresh-install snapshot = (%q, %v), want none", snap, err)
	}
}
