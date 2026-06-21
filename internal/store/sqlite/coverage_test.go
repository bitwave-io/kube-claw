package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/traego/kube-claw/internal/store"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSecretsCRUD(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	tx := func(fn func(store.Tx) error) {
		t.Helper()
		if err := s.Tx(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}

	tx(func(tx store.Tx) error {
		return tx.CreateSecret(store.Secret{ID: "sec-1", Namespace: "ns", Name: "k", Type: "gcp", Granters: []string{"U_A", "U_B"}})
	})
	tx(func(tx store.Tx) error {
		got, err := tx.GetSecret("ns", "k")
		if err != nil {
			return err
		}
		if got.ID != "sec-1" || got.Type != "gcp" || len(got.Granters) != 2 {
			t.Fatalf("secret = %+v", got)
		}
		return nil
	})
	// not found
	tx(func(tx store.Tx) error {
		if _, err := tx.GetSecret("ns", "missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetSecret missing = %v", err)
		}
		// latest version with none yet
		if _, err := tx.LatestSecretVersion("sec-1"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("LatestSecretVersion none = %v", err)
		}
		return nil
	})
	// versions: latest wins
	tx(func(tx store.Tx) error {
		if err := tx.AddSecretVersion(store.SecretVersion{ID: "v1", SecretID: "sec-1", Ciphertext: []byte("a"), CreatedAt: "2026-01-01T00:00:00Z"}); err != nil {
			return err
		}
		return tx.AddSecretVersion(store.SecretVersion{ID: "v2", SecretID: "sec-1", Ciphertext: []byte("b"), CreatedAt: "2026-02-01T00:00:00Z"})
	})
	tx(func(tx store.Tx) error {
		v, err := tx.LatestSecretVersion("sec-1")
		if err != nil {
			return err
		}
		if v.ID != "v2" || string(v.Ciphertext) != "b" {
			t.Fatalf("latest = %+v", v)
		}
		return nil
	})
}

func TestIntakeTokens(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	// valid → consume once
	if err := s.Tx(ctx, func(tx store.Tx) error {
		return tx.CreateIntakeToken("hash-1", "sec-1", "2999-01-01T00:00:00Z")
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.Tx(ctx, func(tx store.Tx) error {
		secID, err := tx.ConsumeIntakeToken("hash-1")
		if err != nil || secID != "sec-1" {
			t.Fatalf("consume = %q, %v", secID, err)
		}
		return nil
	})
	// reuse → ErrTokenUsed
	_ = s.Tx(ctx, func(tx store.Tx) error {
		if _, err := tx.ConsumeIntakeToken("hash-1"); !errors.Is(err, store.ErrTokenUsed) {
			t.Fatalf("reuse = %v, want ErrTokenUsed", err)
		}
		return nil
	})
	// unknown → ErrNotFound
	_ = s.Tx(ctx, func(tx store.Tx) error {
		if _, err := tx.ConsumeIntakeToken("nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown = %v", err)
		}
		return nil
	})
	// expired → ErrTokenExpired
	_ = s.Tx(ctx, func(tx store.Tx) error {
		return tx.CreateIntakeToken("hash-old", "sec-1", "2000-01-01T00:00:00Z")
	})
	_ = s.Tx(ctx, func(tx store.Tx) error {
		if _, err := tx.ConsumeIntakeToken("hash-old"); !errors.Is(err, store.ErrTokenExpired) {
			t.Fatalf("expired = %v, want ErrTokenExpired", err)
		}
		return nil
	})
}

func TestDedupeAndRunStates(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	_ = s.Tx(ctx, func(tx store.Tx) error {
		dup, err := tx.SeenEvent("slack", "evt-1")
		if err != nil || dup {
			t.Fatalf("first SeenEvent = %v, %v", dup, err)
		}
		dup, err = tx.SeenEvent("slack", "evt-1")
		if err != nil || !dup {
			t.Fatalf("second SeenEvent dup = %v, %v", dup, err)
		}
		return nil
	})

	_ = s.Tx(ctx, func(tx store.Tx) error {
		if err := tx.CreateRun(store.Run{ID: "r1", AgentNamespace: "ns", AgentName: "a", Phase: "Pending"}); err != nil {
			return err
		}
		if err := tx.MarkRunBlocked("r1"); err != nil {
			return err
		}
		return tx.MarkRunFailed("r1")
	})
	_ = s.Tx(ctx, func(tx store.Tx) error {
		r, _ := tx.GetRun("r1")
		if r.Phase != "Failed" || r.CompletedAt == "" {
			t.Fatalf("run after MarkRunFailed = %+v", r)
		}
		return nil
	})
}

func TestRequestsAndGrantsListing(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	_ = s.Tx(ctx, func(tx store.Tx) error {
		if err := tx.CreateSecretRequest(store.SecretRequest{ID: "req-1", Status: "Pending", AgentNamespace: "ns", AgentName: "a", SecretID: "sec-1", SecretName: "k"}); err != nil {
			return err
		}
		return tx.CreateGrant(store.Grant{ID: "g1", AgentNamespace: "ns", AgentName: "a", SecretID: "sec-1", ImageDigest: "d", AgentSpecHash: "s", DeliveryHash: "dh"})
	})
	_ = s.Tx(ctx, func(tx store.Tx) error {
		req, err := tx.GetSecretRequest("req-1")
		if err != nil || req.SecretName != "k" {
			t.Fatalf("GetSecretRequest = %+v, %v", req, err)
		}
		all, err := tx.ListSecretRequests("")
		if err != nil || len(all) != 1 {
			t.Fatalf("ListSecretRequests all = %d, %v", len(all), err)
		}
		pending, _ := tx.ListSecretRequests("Pending")
		if len(pending) != 1 {
			t.Fatalf("ListSecretRequests Pending = %d", len(pending))
		}
		grants, err := tx.ListGrants("ns", "a")
		if err != nil || len(grants) != 1 || grants[0].ID != "g1" {
			t.Fatalf("ListGrants = %+v, %v", grants, err)
		}
		return nil
	})
	// GetSecretRequest unknown → ErrNotFound
	_ = s.Tx(ctx, func(tx store.Tx) error {
		if _, err := tx.GetSecretRequest("nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetSecretRequest unknown = %v", err)
		}
		return nil
	})
}
