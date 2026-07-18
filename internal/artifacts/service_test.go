package artifacts

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

func testService(t *testing.T) *Service {
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
	return &Service{Store: st}
}

func TestPublishResolveReshare(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	pub, err := svc.Publish(ctx, "run-1", "th-1", "", "Design", "# Doc\nbody", 0)
	if err != nil {
		t.Fatal(err)
	}
	if pub.ArtifactID == "" || pub.Token == "" {
		t.Fatalf("publish = %+v", pub)
	}
	// Default TTL is 24h.
	if got := time.Until(pub.ExpiresAt); got < 23*time.Hour || got > 25*time.Hour {
		t.Fatalf("default expiry %s from now, want ~24h", got)
	}

	a, exp, err := svc.Resolve(ctx, pub.Token)
	if err != nil {
		t.Fatal(err)
	}
	if a.Content != "# Doc\nbody" || !exp.Equal(pub.ExpiresAt) {
		t.Fatalf("resolve = %+v exp=%s", a, exp)
	}

	// Reshare: same document, fresh token; the old link is revoked.
	re, err := svc.Publish(ctx, "run-2", "th-1", pub.ArtifactID, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if re.ArtifactID != pub.ArtifactID || re.Token == pub.Token {
		t.Fatalf("reshare = %+v", re)
	}
	if _, _, err := svc.Resolve(ctx, pub.Token); !errors.Is(err, store.ErrTokenExpired) {
		t.Fatalf("old token after reshare = %v, want ErrTokenExpired", err)
	}
	if a, _, err := svc.Resolve(ctx, re.Token); err != nil || a.Content != "# Doc\nbody" {
		t.Fatalf("new token resolve = %+v, %v", a, err)
	}

	// Reshare from another session is refused (and reads as not-found).
	if _, err := svc.Publish(ctx, "run-3", "th-OTHER", pub.ArtifactID, "", "", 0); !errors.Is(err, ErrWrongSession) {
		t.Fatalf("cross-session reshare = %v, want ErrWrongSession", err)
	}
	// Unknown artifact id.
	if _, err := svc.Publish(ctx, "run-3", "th-1", "doc-nope", "", "", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown artifact reshare = %v, want ErrNotFound", err)
	}
}

func TestPublishValidation(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	if _, err := svc.Publish(ctx, "run-1", "th-1", "", "", "body", 0); err == nil {
		t.Fatal("empty title accepted")
	}
	if _, err := svc.Publish(ctx, "run-1", "th-1", "", "Title", "  ", 0); err == nil {
		t.Fatal("empty content accepted")
	}
	big := strings.Repeat("x", MaxContentBytes+1)
	if _, err := svc.Publish(ctx, "run-1", "th-1", "", "Title", big, 0); !errors.Is(err, ErrContentTooLarge) {
		t.Fatalf("oversize content = %v, want ErrContentTooLarge", err)
	}
}

func TestTTLClamp(t *testing.T) {
	svc := testService(t)
	svc.TTL, svc.MaxTTL = 2*time.Hour, 4*time.Hour
	ctx := context.Background()

	pub, err := svc.Publish(ctx, "run-1", "th-1", "", "T", "c", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Until(pub.ExpiresAt); got > 2*time.Hour+time.Minute {
		t.Fatalf("default ttl produced %s, want ≤2h", got)
	}
	pub, err = svc.Publish(ctx, "run-1", "th-1", "", "T2", "c", 100*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Until(pub.ExpiresAt); got > 4*time.Hour+time.Minute {
		t.Fatalf("override ttl clamped to %s, want ≤4h", got)
	}
}
