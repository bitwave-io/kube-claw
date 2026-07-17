package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/traego/kube-claw/internal/store"
)

func TestGrantsAndRequests(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	const (
		ns, agent, secretID     = "claw-agents", "gcp-cost", "sec-1"
		digest, specHash, dHash = "sha256:img", "sha256:spec", "sha256:deliv"
	)
	tx := func(fn func(store.Tx) error) {
		t.Helper()
		if err := s.Tx(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}

	// No grant yet.
	tx(func(tx store.Tx) error {
		if _, err := tx.FindValidGrant(ns, agent, secretID, digest, specHash, dHash); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
		return nil
	})

	// Request dedupe.
	tx(func(tx store.Tx) error {
		exists, _ := tx.PendingRequestExists(ns, agent, secretID)
		if exists {
			t.Fatal("unexpected pending request")
		}
		return tx.CreateSecretRequest(store.SecretRequest{ID: "req-1", Status: "Pending", AgentNamespace: ns, AgentName: agent, SecretID: secretID, SecretName: "gcp-billing"})
	})
	tx(func(tx store.Tx) error {
		exists, _ := tx.PendingRequestExists(ns, agent, secretID)
		if !exists {
			t.Fatal("expected pending request to exist")
		}
		return nil
	})

	// Approve → grant exists and is found by the exact binding.
	tx(func(tx store.Tx) error {
		if err := tx.CreateGrant(store.Grant{
			ID: "grant-1", AgentNamespace: ns, AgentName: agent, SecretID: secretID,
			ImageDigest: digest, AgentSpecHash: specHash, DeliveryHash: dHash, ApprovedBy: "alex",
		}); err != nil {
			return err
		}
		return tx.SetSecretRequestStatus("req-1", "Approved")
	})
	tx(func(tx store.Tx) error {
		if _, err := tx.FindValidGrant(ns, agent, secretID, digest, specHash, dHash); err != nil {
			t.Fatalf("grant not found after approve: %v", err)
		}
		// A different digest (image changed) must NOT match → re-approval needed.
		if _, err := tx.FindValidGrant(ns, agent, secretID, "sha256:NEW", specHash, dHash); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("grant matched a changed image digest: %v", err)
		}
		return nil
	})

	// Revoke → no longer valid.
	tx(func(tx store.Tx) error {
		if err := tx.RevokeGrant("grant-1", "rotated"); err != nil {
			return err
		}
		if _, err := tx.FindValidGrant(ns, agent, secretID, digest, specHash, dHash); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("revoked grant still valid: %v", err)
		}
		return nil
	})
}
