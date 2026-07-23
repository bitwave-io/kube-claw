package slack

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/traego/kube-claw/internal/store/sqlite"
)

func newUpgradeTestRouter(t *testing.T) *Router {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return &Router{Store: st}
}

// TestAdminClaimFirstWins: the first claimer becomes the upgrade admin; a
// second claim is refused and reports the incumbent (DESIGN.md §24.6).
func TestAdminClaimFirstWins(t *testing.T) {
	ctx := context.Background()
	r := newUpgradeTestRouter(t)

	if _, ok := r.UpgradeAdmin(ctx); ok {
		t.Fatal("expected no admin before claim")
	}
	if msg := r.HandleAdminClaim(ctx, "U_FIRST"); !strings.Contains(msg, "U_FIRST") || !strings.Contains(msg, "✅") {
		t.Fatalf("first claim reply = %q", msg)
	}
	if msg := r.HandleAdminClaim(ctx, "U_SECOND"); !strings.Contains(msg, "already") || !strings.Contains(msg, "U_FIRST") {
		t.Fatalf("second claim reply = %q, want refusal naming U_FIRST", msg)
	}
	if admin, ok := r.UpgradeAdmin(ctx); !ok || admin != "U_FIRST" {
		t.Fatalf("UpgradeAdmin = (%q, %v), want (U_FIRST, true)", admin, ok)
	}
}

// fakeUpgrades records upgrade decisions.
type fakeUpgrades struct {
	approved, skipped, deferred string
	checks                      int
}

func (f *fakeUpgrades) Approve(_ context.Context, v, _ string) error { f.approved = v; return nil }
func (f *fakeUpgrades) Skip(_ context.Context, v, _ string) error    { f.skipped = v; return nil }
func (f *fakeUpgrades) Later(_ context.Context, v string) error      { f.deferred = v; return nil }
func (f *fakeUpgrades) CheckNow(_ context.Context) error             { f.checks++; return nil }

// TestHandleUpgradeAction: only the configured admin's clicks are honored, and
// each action dispatches to the UpgradeActor.
func TestHandleUpgradeAction(t *testing.T) {
	ctx := context.Background()
	r := newUpgradeTestRouter(t)
	fake := &fakeUpgrades{}
	r.Upgrades = fake
	r.HandleAdminClaim(ctx, "U_ADMIN")

	// Non-admin click → refused, nothing recorded.
	if msg := r.HandleUpgradeAction(ctx, UpgradeActionValue("approve", "v0.4.0"), "U_RANDO"); !strings.Contains(msg, "not the upgrade admin") {
		t.Fatalf("non-admin reply = %q", msg)
	}
	if fake.approved != "" {
		t.Fatal("non-admin approve must not dispatch")
	}

	if msg := r.HandleUpgradeAction(ctx, UpgradeActionValue("approve", "v0.4.0"), "U_ADMIN"); !strings.Contains(msg, "Upgrading") {
		t.Fatalf("approve reply = %q", msg)
	}
	if fake.approved != "v0.4.0" {
		t.Fatalf("approved = %q, want v0.4.0", fake.approved)
	}
	r.HandleUpgradeAction(ctx, UpgradeActionValue("skip", "v0.5.0"), "U_ADMIN")
	if fake.skipped != "v0.5.0" {
		t.Fatalf("skipped = %q, want v0.5.0", fake.skipped)
	}
	r.HandleUpgradeAction(ctx, UpgradeActionValue("later", "v0.6.0"), "U_ADMIN")
	if fake.deferred != "v0.6.0" {
		t.Fatalf("deferred = %q, want v0.6.0", fake.deferred)
	}

	// Garbage value → no dispatch, graceful reply.
	if msg := r.HandleUpgradeAction(ctx, "upgrade|nonsense", "U_ADMIN"); msg != "unrecognized action" {
		t.Fatalf("garbage reply = %q", msg)
	}

	// No actor configured → graceful reply.
	r.Upgrades = nil
	if msg := r.HandleUpgradeAction(ctx, UpgradeActionValue("approve", "v0.4.0"), "U_ADMIN"); !strings.Contains(msg, "isn't configured") {
		t.Fatalf("nil-actor reply = %q", msg)
	}
}
