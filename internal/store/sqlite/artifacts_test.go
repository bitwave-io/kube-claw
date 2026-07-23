package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/traego/kube-claw/internal/store"
)

func TestArtifactsAndShareTokens(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tx := func(fn func(store.Tx) error) {
		t.Helper()
		if err := s.Tx(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}

	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)

	tx(func(tx store.Tx) error {
		if _, err := tx.GetArtifact("doc-1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetArtifact before create = %v, want ErrNotFound", err)
		}
		return tx.CreateArtifact(store.Artifact{
			ID: "doc-1", RunID: "run-1", SessionID: "th-1",
			Title: "Billing-alerts design", Content: "# Design\nbody",
		})
	})
	tx(func(tx store.Tx) error {
		a, err := tx.GetArtifact("doc-1")
		if err != nil {
			return err
		}
		if a.Title != "Billing-alerts design" || a.Content != "# Design\nbody" || a.SessionID != "th-1" {
			t.Fatalf("artifact round-trip = %+v", a)
		}
		return nil
	})

	// Live token: resolvable, and multi-read (resolving does not consume).
	tx(func(tx store.Tx) error { return tx.CreateArtifactToken("hash-live", "doc-1", future) })
	for i := 0; i < 2; i++ {
		tx(func(tx store.Tx) error {
			a, exp, err := tx.ResolveArtifactToken("hash-live")
			if err != nil {
				return err
			}
			if a.ID != "doc-1" || exp != future {
				t.Fatalf("resolve #%d = %q exp=%q", i, a.ID, exp)
			}
			return nil
		})
	}

	// Expired token.
	tx(func(tx store.Tx) error { return tx.CreateArtifactToken("hash-old", "doc-1", past) })
	tx(func(tx store.Tx) error {
		if _, exp, err := tx.ResolveArtifactToken("hash-old"); !errors.Is(err, store.ErrTokenExpired) || exp != past {
			t.Fatalf("expired resolve = exp=%q err=%v, want ErrTokenExpired", exp, err)
		}
		return nil
	})

	// Unknown token.
	tx(func(tx store.Tx) error {
		if _, _, err := tx.ResolveArtifactToken("hash-nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown resolve = %v, want ErrNotFound", err)
		}
		return nil
	})

	// Revoke kills the live token too.
	tx(func(tx store.Tx) error { return tx.RevokeArtifactTokens("doc-1") })
	tx(func(tx store.Tx) error {
		if _, _, err := tx.ResolveArtifactToken("hash-live"); !errors.Is(err, store.ErrTokenExpired) {
			t.Fatalf("revoked resolve = %v, want ErrTokenExpired", err)
		}
		return nil
	})

	// ArtifactIDByTokenHash resolves expired AND revoked tokens (reshare-by-old-link).
	tx(func(tx store.Tx) error {
		for _, h := range []string{"hash-live", "hash-old"} {
			if id, err := tx.ArtifactIDByTokenHash(h); err != nil || id != "doc-1" {
				t.Fatalf("ArtifactIDByTokenHash(%q) = %q, %v", h, id, err)
			}
		}
		if _, err := tx.ArtifactIDByTokenHash("hash-nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown hash = %v, want ErrNotFound", err)
		}
		return nil
	})
}

func TestListArtifacts(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	tx := func(fn func(store.Tx) error) {
		t.Helper()
		if err := s.Tx(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}

	tx(func(tx store.Tx) error {
		for _, a := range []store.Artifact{
			{ID: "doc-1", RunID: "run-1", SessionID: "th-1", Title: "First", Content: "1", CreatedAt: "2026-07-17T10:00:00Z"},
			{ID: "doc-2", RunID: "run-2", SessionID: "th-1", Title: "Second", Content: "2", CreatedAt: "2026-07-18T10:00:00Z"},
			{ID: "doc-3", RunID: "run-3", SessionID: "th-2", Title: "Other thread", Content: "3", CreatedAt: "2026-07-18T11:00:00Z"},
			{ID: "doc-4", RunID: "run-cli", SessionID: "", Title: "CLI doc", Content: "4", CreatedAt: "2026-07-18T12:00:00Z"},
		} {
			if err := tx.CreateArtifact(a); err != nil {
				return err
			}
		}
		return nil
	})

	tx(func(tx store.Tx) error {
		docs, err := tx.ListArtifacts("th-1", "run-ignored")
		if err != nil {
			return err
		}
		if len(docs) != 2 || docs[0].ID != "doc-1" || docs[1].ID != "doc-2" {
			t.Fatalf("session list = %+v", docs)
		}
		if docs[0].Title != "First" || docs[0].Content != "" {
			t.Fatalf("list entry = %+v, want metadata only", docs[0])
		}
		// Session-less scoping falls back to the run — no cross-CLI leakage.
		if docs, err := tx.ListArtifacts("", "run-cli"); err != nil || len(docs) != 1 || docs[0].ID != "doc-4" {
			t.Fatalf("CLI list = %+v, %v", docs, err)
		}
		if docs, err := tx.ListArtifacts("", "run-1"); err != nil || len(docs) != 0 {
			t.Fatalf("CLI list for session run = %+v, %v", docs, err)
		}
		return nil
	})
}
