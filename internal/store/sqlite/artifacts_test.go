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
}
