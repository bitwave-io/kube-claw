package secrets

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/traego/kube-claw/internal/store"
)

// Service is the secret authority: it encrypts on write, decrypts on read, and
// manages one-time intake tokens. The plaintext value never leaves this layer
// except via GetValue (used by the materialize path, Phase 5).
type Service struct {
	Store    store.Store
	Cipher   Cipher
	TokenTTL time.Duration // intake link lifetime (default 15m)
}

// CreateSecret records metadata + granters (no value yet) and audits.
func (s *Service) CreateSecret(ctx context.Context, namespace, name, typ, description string, granters []string) (store.Secret, error) {
	sec := store.Secret{
		ID:          newID("sec"),
		Namespace:   namespace,
		Name:        name,
		Type:        typ,
		Description: description,
		Granters:    granters,
		CreatedAt:   store.NowRFC3339(),
	}
	err := s.Store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.CreateSecret(sec); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "secret.created", SecretID: sec.ID, Actor: "cli"})
	})
	return sec, err
}

// PutValue encrypts + stores a new version of a named secret (break-glass / CI).
func (s *Service) PutValue(ctx context.Context, namespace, name string, plaintext []byte, createdBy string) error {
	return s.Store.Tx(ctx, func(tx store.Tx) error {
		sec, err := tx.GetSecret(namespace, name)
		if err != nil {
			return err
		}
		return s.addVersion(tx, sec.ID, plaintext, createdBy)
	})
}

// GetValue decrypts + returns the latest version of a named secret. Used by the
// materialize path; callers MUST NOT log the result.
func (s *Service) GetValue(ctx context.Context, namespace, name string) ([]byte, error) {
	var out []byte
	err := s.Store.Tx(ctx, func(tx store.Tx) error {
		sec, err := tx.GetSecret(namespace, name)
		if err != nil {
			return err
		}
		v, err := tx.LatestSecretVersion(sec.ID)
		if err != nil {
			return err
		}
		pt, err := s.Cipher.Decrypt(v.Ciphertext, []byte(sec.ID))
		if err != nil {
			return fmt.Errorf("decrypt secret: %w", err)
		}
		out = pt
		return nil
	})
	return out, err
}

// MintIntakeToken creates a one-time intake link token for a secret and returns
// the RAW token (only the hash is stored). The caller embeds it in the URL.
func (s *Service) MintIntakeToken(ctx context.Context, namespace, name string) (string, error) {
	ttl := s.TokenTTL
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	raw := randomToken()
	expires := time.Now().UTC().Add(ttl).Format(time.RFC3339Nano)

	err := s.Store.Tx(ctx, func(tx store.Tx) error {
		sec, err := tx.GetSecret(namespace, name)
		if err != nil {
			return err
		}
		return tx.CreateIntakeToken(hashToken(raw), sec.ID, expires)
	})
	if err != nil {
		return "", err
	}
	return raw, nil
}

// SubmitIntake consumes a token (single-use) and stores the submitted value.
func (s *Service) SubmitIntake(ctx context.Context, rawToken string, plaintext []byte) error {
	return s.Store.Tx(ctx, func(tx store.Tx) error {
		secretID, err := tx.ConsumeIntakeToken(hashToken(rawToken))
		if err != nil {
			return err
		}
		return s.addVersion(tx, secretID, plaintext, "intake-ui")
	})
}

// addVersion encrypts plaintext (AAD = secret id) and stores a version + audit.
func (s *Service) addVersion(tx store.Tx, secretID string, plaintext []byte, createdBy string) error {
	ct, err := s.Cipher.Encrypt(plaintext, []byte(secretID))
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	sum := sha256.Sum256(plaintext)
	if err := tx.AddSecretVersion(store.SecretVersion{
		ID:         newID("sv"),
		SecretID:   secretID,
		Ciphertext: ct,
		Checksum:   hex.EncodeToString(sum[:]),
		CreatedBy:  createdBy,
	}); err != nil {
		return err
	}
	return tx.AppendAudit(store.AuditEvent{Type: "secret.version_added", SecretID: secretID, Actor: createdBy})
}

// --- helpers ---

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
