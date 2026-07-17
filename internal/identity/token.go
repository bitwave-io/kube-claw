// Package identity is the pluggable agent-identity layer (DESIGN.md §9): a
// /login token exchange verifies a platform credential (Kubernetes SA token by
// default) and issues a short-lived, claw-signed session token scoped to a run
// and its approved secrets. The runner uses the claw token, never the raw SA token.
package identity

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// Claims is what a claw session token carries.
type Claims struct {
	RunID   string   `json:"run"`
	Secrets []string `json:"secrets"`        // secret names this token may materialize
	Exp     int64    `json:"exp"`            // unix seconds
	Kind    string   `json:"kind,omitempty"` // "" or KindAccess = access; KindRefresh = refresh-only
}

// Token kinds. An access token authenticates runner callbacks; a refresh token
// is only accepted by the token-refresh exchange. Empty kind means access
// (tokens issued before kinds existed).
const (
	KindAccess  = "access"
	KindRefresh = "refresh"
)

// Signer issues + verifies claw session tokens (HMAC-SHA256). The key is random
// per process; tokens are short-lived (minutes) so a restart simply forces a
// re-login, which is fine.
type Signer struct{ key []byte }

func NewSigner() (*Signer, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &Signer{key: key}, nil
}

var b64 = base64.RawURLEncoding

// Issue returns a signed access token: base64(claims).base64(hmac).
func (s *Signer) Issue(runID string, secrets []string, ttl time.Duration) (string, error) {
	return s.issue(Claims{RunID: runID, Secrets: secrets, Exp: time.Now().Add(ttl).Unix(), Kind: KindAccess})
}

// IssueRefresh returns a signed refresh token. It carries no secret scopes —
// scopes are re-derived from current grants at refresh time.
func (s *Signer) IssueRefresh(runID string, ttl time.Duration) (string, error) {
	return s.issue(Claims{RunID: runID, Exp: time.Now().Add(ttl).Unix(), Kind: KindRefresh})
}

func (s *Signer) issue(c Claims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	p := b64.EncodeToString(payload)
	return p + "." + b64.EncodeToString(s.mac([]byte(p))), nil
}

// Verify checks the signature + expiry and returns the claims of an ACCESS
// token. Refresh tokens are rejected here — they are only good for the refresh
// exchange (VerifyRefresh), never as a bearer credential.
func (s *Signer) Verify(token string) (Claims, error) {
	c, err := s.verify(token)
	if err != nil {
		return c, err
	}
	if c.Kind == KindRefresh {
		return c, fmt.Errorf("refresh token used as access token")
	}
	return c, nil
}

// VerifyRefresh checks the signature + expiry of a REFRESH token.
func (s *Signer) VerifyRefresh(token string) (Claims, error) {
	c, err := s.verify(token)
	if err != nil {
		return c, err
	}
	if c.Kind != KindRefresh {
		return c, fmt.Errorf("not a refresh token")
	}
	return c, nil
}

func (s *Signer) verify(token string) (Claims, error) {
	var c Claims
	var p, sig string
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			p, sig = token[:i], token[i+1:]
			break
		}
	}
	if p == "" || sig == "" {
		return c, fmt.Errorf("malformed token")
	}
	want, err := b64.DecodeString(sig)
	if err != nil || !hmac.Equal(want, s.mac([]byte(p))) {
		return c, fmt.Errorf("bad signature")
	}
	payload, err := b64.DecodeString(p)
	if err != nil {
		return c, fmt.Errorf("bad payload")
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, err
	}
	if time.Now().Unix() > c.Exp {
		return c, fmt.Errorf("token expired")
	}
	return c, nil
}

// Allows reports whether the claims permit materializing the named secret.
func (c Claims) Allows(secret string) bool {
	for _, s := range c.Secrets {
		if s == secret {
			return true
		}
	}
	return false
}

func (s *Signer) mac(b []byte) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write(b)
	return m.Sum(nil)
}
