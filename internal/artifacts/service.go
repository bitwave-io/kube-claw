// Package artifacts publishes agent-produced documents (design docs) behind
// time-bound share links, so a Slack design conversation can hand a stable
// artifact to tooling outside Slack (e.g. a local coding agent fetching the URL).
//
// The link mechanics mirror the secret-intake tokens (256-bit CSPRNG, only the
// SHA-256 hash stored) with two deliberate differences: share tokens are
// MULTI-READ until expiry (the human previews it, then their agent fetches it),
// and the document outlives its links — resharing mints a fresh token on the
// same immutable artifact and revokes the old ones.
package artifacts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/traego/kube-claw/internal/store"
)

// MaxContentBytes caps a published document (these are design docs, not dumps).
const MaxContentBytes = 1 << 20 // 1 MiB

// ErrContentTooLarge is returned by Publish when content exceeds MaxContentBytes.
var ErrContentTooLarge = fmt.Errorf("artifacts: content exceeds %d bytes", MaxContentBytes)

// ErrWrongSession is returned when a reshare names an artifact from another session.
var ErrWrongSession = errors.New("artifacts: artifact belongs to a different session")

// Service mints and resolves time-bound share links for published documents.
type Service struct {
	Store  store.Store
	TTL    time.Duration // default link lifetime (default 24h)
	MaxTTL time.Duration // cap on per-publish TTL overrides (default 7d)
}

// Published is the result of a Publish: everything the agent needs to hand the
// user a link and be explicit about when it dies.
type Published struct {
	ArtifactID string
	Token      string // RAW token (only the hash is stored) — embed in the URL
	ExpiresAt  time.Time
}

// Publish stores a new document and mints a share token for it. If artifactID
// names an existing artifact (a reshare), the stored content is kept, its live
// tokens are revoked, and a fresh token is minted — but only when the artifact
// belongs to the caller's session, so one thread's agent can't relink another's.
// ttl <= 0 uses the default; anything above MaxTTL is clamped.
func (s *Service) Publish(ctx context.Context, runID, sessionID, artifactID, title, content string, ttl time.Duration) (Published, error) {
	ttl = s.clampTTL(ttl)
	raw := randomToken()
	expires := time.Now().UTC().Add(ttl)

	pub := Published{ArtifactID: artifactID, Token: raw, ExpiresAt: expires}
	err := s.Store.Tx(ctx, func(tx store.Tx) error {
		if artifactID != "" {
			a, err := tx.GetArtifact(artifactID)
			if err != nil {
				return err
			}
			if a.SessionID != sessionID {
				return ErrWrongSession
			}
			if err := tx.RevokeArtifactTokens(artifactID); err != nil {
				return err
			}
			if err := tx.CreateArtifactToken(hashToken(raw), artifactID, expires.Format(time.RFC3339Nano)); err != nil {
				return err
			}
			return tx.AppendAudit(store.AuditEvent{Type: "artifact.reshared", RunID: runID,
				Detail: map[string]any{"artifact": artifactID, "expiresAt": expires.Format(time.RFC3339)}})
		}
		if strings.TrimSpace(title) == "" || strings.TrimSpace(content) == "" {
			return errors.New("artifacts: title and content are required")
		}
		if len(content) > MaxContentBytes {
			return ErrContentTooLarge
		}
		a := store.Artifact{
			ID:        newID("doc"),
			RunID:     runID,
			SessionID: sessionID,
			Title:     title,
			Content:   content,
			CreatedAt: store.NowRFC3339(),
		}
		if err := tx.CreateArtifact(a); err != nil {
			return err
		}
		pub.ArtifactID = a.ID
		if err := tx.CreateArtifactToken(hashToken(raw), a.ID, expires.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "artifact.published", RunID: runID,
			Detail: map[string]any{"artifact": a.ID, "title": title, "bytes": len(content),
				"expiresAt": expires.Format(time.RFC3339)}})
	})
	if err != nil {
		return Published{}, err
	}
	return pub, nil
}

// Resolve returns the artifact behind a raw share token plus the link's expiry.
// Returns store.ErrNotFound for an unknown token, store.ErrTokenExpired for an
// expired or revoked one (with the expiry still set, for the 410 message).
func (s *Service) Resolve(ctx context.Context, rawToken string) (store.Artifact, time.Time, error) {
	var a store.Artifact
	var expiresRaw string
	err := s.Store.Tx(ctx, func(tx store.Tx) error {
		got, exp, e := tx.ResolveArtifactToken(hashToken(rawToken))
		a, expiresRaw = got, exp
		return e
	})
	expires, _ := time.Parse(time.RFC3339Nano, expiresRaw)
	return a, expires, err
}

func (s *Service) clampTTL(ttl time.Duration) time.Duration {
	def, max := s.TTL, s.MaxTTL
	if def <= 0 {
		def = 24 * time.Hour
	}
	if max <= 0 {
		max = 7 * 24 * time.Hour
	}
	if ttl <= 0 {
		ttl = def
	}
	if ttl > max {
		ttl = max
	}
	return ttl
}

// --- helpers (same shapes as internal/secrets) ---

func newID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

func randomToken() string {
	b := make([]byte, 32) // 256-bit
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
