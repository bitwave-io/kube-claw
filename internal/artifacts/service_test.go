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

	pub, err := svc.Publish(ctx, "run-1", "th-1", "", "", "Design", "# Doc\nbody", 0)
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
	re, err := svc.Publish(ctx, "run-2", "th-1", pub.ArtifactID, "", "", "", 0)
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
	if _, err := svc.Publish(ctx, "run-3", "th-OTHER", pub.ArtifactID, "", "", "", 0); !errors.Is(err, ErrWrongSession) {
		t.Fatalf("cross-session reshare = %v, want ErrWrongSession", err)
	}
	// Unknown artifact id.
	if _, err := svc.Publish(ctx, "run-3", "th-1", "doc-nope", "", "", "", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown artifact reshare = %v, want ErrNotFound", err)
	}
}

func TestReshareByShareToken(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	pub, err := svc.Publish(ctx, "run-1", "th-1", "", "", "Design", "# Doc\nbody", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Reshare names the document by a previous link's token instead of the id.
	re, err := svc.Publish(ctx, "run-2", "th-1", "", pub.Token, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if re.ArtifactID != pub.ArtifactID || re.Token == pub.Token {
		t.Fatalf("token reshare = %+v", re)
	}
	if _, _, err := svc.Resolve(ctx, pub.Token); !errors.Is(err, store.ErrTokenExpired) {
		t.Fatalf("old token after token reshare = %v, want ErrTokenExpired", err)
	}

	// The point of the feature: the OLD, now-revoked token still names the
	// document for a reshare (it's the only handle a rebuilt session has).
	re2, err := svc.Publish(ctx, "run-3", "th-1", "", pub.Token, "", "", 0)
	if err != nil {
		t.Fatalf("reshare via revoked token = %v", err)
	}
	if re2.ArtifactID != pub.ArtifactID {
		t.Fatalf("revoked-token reshare = %+v", re2)
	}
	if a, _, err := svc.Resolve(ctx, re2.Token); err != nil || a.Content != "# Doc\nbody" {
		t.Fatalf("new token resolve = %+v, %v", a, err)
	}

	// Unknown token, and a token belonging to another session's document.
	if _, err := svc.Publish(ctx, "run-3", "th-1", "", "not-a-token", "", "", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown token reshare = %v, want ErrNotFound", err)
	}
	if _, err := svc.Publish(ctx, "run-4", "th-OTHER", "", pub.Token, "", "", 0); !errors.Is(err, ErrWrongSession) {
		t.Fatalf("cross-session token reshare = %v, want ErrWrongSession", err)
	}
	// An explicit artifact_id wins over a share token pointing elsewhere.
	other, err := svc.Publish(ctx, "run-1", "th-1", "", "", "Other", "x", 0)
	if err != nil {
		t.Fatal(err)
	}
	re3, err := svc.Publish(ctx, "run-5", "th-1", other.ArtifactID, pub.Token, "", "", 0)
	if err != nil || re3.ArtifactID != other.ArtifactID {
		t.Fatalf("id+token reshare = %+v, %v", re3, err)
	}
}

func TestList(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	if docs, err := svc.List(ctx, "th-1", "run-1"); err != nil || len(docs) != 0 {
		t.Fatalf("empty list = %v, %v", docs, err)
	}
	a, _ := svc.Publish(ctx, "run-1", "th-1", "", "", "First", "1", 0)
	b, _ := svc.Publish(ctx, "run-2", "th-1", "", "", "Second", "2", 0)
	if _, err := svc.Publish(ctx, "run-9", "th-OTHER", "", "", "Elsewhere", "x", 0); err != nil {
		t.Fatal(err)
	}
	// CLI (session-less) documents are scoped to their own run.
	if _, err := svc.Publish(ctx, "run-cli", "", "", "", "CLI doc", "x", 0); err != nil {
		t.Fatal(err)
	}

	docs, err := svc.List(ctx, "th-1", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].ID != a.ArtifactID || docs[1].ID != b.ArtifactID {
		t.Fatalf("session list = %+v", docs)
	}
	if docs[0].Title != "First" || docs[0].Content != "" {
		t.Fatalf("list entry = %+v, want title only (no content)", docs[0])
	}
	// Session-less list sees only its own run's documents.
	docs, err = svc.List(ctx, "", "run-cli")
	if err != nil || len(docs) != 1 || docs[0].Title != "CLI doc" {
		t.Fatalf("CLI list = %+v, %v", docs, err)
	}
	docs, err = svc.List(ctx, "", "run-other-cli")
	if err != nil || len(docs) != 0 {
		t.Fatalf("other CLI list = %+v, %v", docs, err)
	}
}

func TestPublishValidation(t *testing.T) {
	svc := testService(t)
	ctx := context.Background()

	if _, err := svc.Publish(ctx, "run-1", "th-1", "", "", "", "body", 0); err == nil {
		t.Fatal("empty title accepted")
	}
	if _, err := svc.Publish(ctx, "run-1", "th-1", "", "", "Title", "  ", 0); err == nil {
		t.Fatal("empty content accepted")
	}
	big := strings.Repeat("x", MaxContentBytes+1)
	if _, err := svc.Publish(ctx, "run-1", "th-1", "", "", "Title", big, 0); !errors.Is(err, ErrContentTooLarge) {
		t.Fatalf("oversize content = %v, want ErrContentTooLarge", err)
	}
}

func TestTTLClamp(t *testing.T) {
	svc := testService(t)
	svc.TTL, svc.MaxTTL = 2*time.Hour, 4*time.Hour
	ctx := context.Background()

	pub, err := svc.Publish(ctx, "run-1", "th-1", "", "", "T", "c", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Until(pub.ExpiresAt); got > 2*time.Hour+time.Minute {
		t.Fatalf("default ttl produced %s, want ≤2h", got)
	}
	pub, err = svc.Publish(ctx, "run-1", "th-1", "", "", "T2", "c", 100*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Until(pub.ExpiresAt); got > 4*time.Hour+time.Minute {
		t.Fatalf("override ttl clamped to %s, want ≤4h", got)
	}
}
