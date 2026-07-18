package sqlite

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// versionMarker records which release last migrated the DB. Written AFTER a
// successful migrate (WriteVersionMarker), read BEFORE the next boot's
// migrate — a mismatch means "this boot may migrate" and triggers a snapshot.
const versionMarker = ".claw-version"

// snapshotsKept bounds pre-migration snapshot count (oldest pruned; retention
// policy ties into TODOS T-2).
const snapshotsKept = 2

// SnapshotBeforeMigrate copies the SQLite file to <db>.pre-<version> when the
// running version differs from the one that last migrated (DESIGN.md §24.5):
// the PVC-local restore point for a failed migration release, taken BEFORE
// sqlite.Open so the copy is of a quiesced file. Returns the snapshot path
// ("" when no snapshot was needed).
func SnapshotBeforeMigrate(dataDir, dbFile, version string) (string, error) {
	if version == "" {
		return "", nil
	}
	prev, err := os.ReadFile(filepath.Join(dataDir, versionMarker))
	if err == nil && strings.TrimSpace(string(prev)) == version {
		return "", nil // same release already migrated this DB
	}
	src := filepath.Join(dataDir, dbFile)
	if _, err := os.Stat(src); err != nil {
		return "", nil // fresh install — nothing to snapshot
	}
	dst := src + ".pre-" + sanitize(version)
	if err := copyFile(src, dst); err != nil {
		return "", fmt.Errorf("pre-migration snapshot: %w", err)
	}
	pruneSnapshots(src)
	return dst, nil
}

// WriteVersionMarker records the version whose migrations completed. Call
// after Migrate succeeds.
func WriteVersionMarker(dataDir, version string) error {
	return os.WriteFile(filepath.Join(dataDir, versionMarker), []byte(version), 0o600)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// pruneSnapshots keeps only the newest snapshotsKept <db>.pre-* files.
func pruneSnapshots(dbPath string) {
	matches, err := filepath.Glob(dbPath + ".pre-*")
	if err != nil || len(matches) <= snapshotsKept {
		return
	}
	sort.Slice(matches, func(i, j int) bool {
		si, ei := os.Stat(matches[i])
		sj, ej := os.Stat(matches[j])
		if ei != nil || ej != nil {
			return matches[i] < matches[j]
		}
		return si.ModTime().After(sj.ModTime()) // newest first
	})
	for _, old := range matches[snapshotsKept:] {
		_ = os.Remove(old)
	}
}

// sanitize keeps snapshot suffixes filesystem-safe.
func sanitize(v string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			return r
		default:
			return '_'
		}
	}, v)
}
