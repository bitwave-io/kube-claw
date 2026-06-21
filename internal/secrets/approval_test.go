package secrets

import (
	"context"
	"testing"

	"github.com/traego/kube-claw/internal/store"
)

func TestApproveDenyRevoke(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	sec, err := svc.CreateSecret(ctx, "claw-agents", "gcp-billing", "gcp", "", []string{"U_ALEX"})
	if err != nil {
		t.Fatal(err)
	}
	mkReq := func(id string) {
		if err := svc.Store.Tx(ctx, func(tx store.Tx) error {
			return tx.CreateSecretRequest(store.SecretRequest{
				ID: id, Status: "Pending", AgentNamespace: "claw-agents", AgentName: "gcp-cost",
				SecretID: sec.ID, SecretName: "gcp-billing",
			})
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Approve → durable grant matching the binding.
	mkReq("req-1")
	b := GrantBinding{ImageDigest: "sha256:img", AgentSpecHash: "sha256:spec", DeliveryHash: "sha256:dh", ServiceAccount: "claw-agent-gcp-cost"}
	grant, err := svc.ApproveRequest(ctx, "req-1", "alex", "ok", b)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := svc.Store.Tx(ctx, func(tx store.Tx) error {
		if _, e := tx.FindValidGrant("claw-agents", "gcp-cost", sec.ID, "sha256:img", "sha256:spec", "sha256:dh"); e != nil {
			t.Fatalf("grant not found: %v", e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Approving an already-approved request fails.
	if _, err := svc.ApproveRequest(ctx, "req-1", "alex", "again", b); err == nil {
		t.Fatal("expected error approving non-pending request")
	}

	// Deny a fresh request.
	mkReq("req-2")
	if err := svc.DenyRequest(ctx, "req-2", "alex", "no"); err != nil {
		t.Fatalf("deny: %v", err)
	}

	// Revoke the grant → no longer valid.
	if err := svc.RevokeGrant(ctx, grant.ID, "alex", "rotating"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_ = svc.Store.Tx(ctx, func(tx store.Tx) error {
		if _, e := tx.FindValidGrant("claw-agents", "gcp-cost", sec.ID, "sha256:img", "sha256:spec", "sha256:dh"); e == nil {
			t.Fatal("revoked grant still valid")
		}
		return nil
	})
}
