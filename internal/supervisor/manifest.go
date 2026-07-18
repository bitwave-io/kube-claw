package supervisor

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/version"
)

// Manifest is the published release manifest (DESIGN.md §24.3): one HTTPS GET,
// digest-pinned image refs, and the flags that gate self-application. A
// detached ed25519 signature at <url>.sig is verified when a public key is
// configured (T-9, FetchManifestSigned).
type Manifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Channel       string `json:"channel"`
	Version       string `json:"version"`
	ReleasedAt    string `json:"releasedAt,omitempty"`
	Images        struct {
		Controller string `json:"controller"`
		Runner     string `json:"runner"`
	} `json:"images"`
	MinSupervisorVersion string `json:"minSupervisorVersion,omitempty"`
	RequiresHelmUpgrade  bool   `json:"requiresHelmUpgrade,omitempty"`
	ContainsMigration    bool   `json:"containsMigration,omitempty"`
	Notes                string `json:"notes,omitempty"`
}

// DefaultManifestURL is the per-channel release manifest location. GitHub's
// releases/latest/download/ always serves the newest release's asset, so the
// URL itself is stable.
func DefaultManifestURL(channel string) string {
	if channel == "" {
		channel = "stable"
	}
	// NOTE: the GitHub org is bitwave-io — NOT the Go module path (traego).
	// The mismatch shipped in v0.4.0 as a 404 and was caught on the first
	// production poll; installs on v0.4.0 need updates.manifestURL set.
	return fmt.Sprintf("https://github.com/bitwave-io/kube-claw/releases/latest/download/manifest-%s.json", channel)
}

// FetchManifest GETs and validates a release manifest, unsigned (no key
// configured). Prefer FetchManifestSigned when a trust anchor exists.
func FetchManifest(ctx context.Context, url string) (Manifest, error) {
	return FetchManifestSigned(ctx, url, nil)
}

// FetchManifestSigned GETs, verifies, and validates a release manifest (T-9).
// With public keys configured, the detached signature at <url>.sig MUST verify
// over the exact manifest bytes with AT LEAST ONE key — a missing or invalid
// signature rejects the manifest (fail closed). Multiple keys form a rotation
// ring: installs carry old+new during a key rollover, so rotation is never a
// flag day. In auto mode the manifest endpoint is release authority, so the
// trust anchor must not ride the same channel: the keys come from Helm values
// (env), never from the manifest or its host.
func FetchManifestSigned(ctx context.Context, url string, pubKeys []ed25519.PublicKey) (Manifest, error) {
	raw, err := fetchBytes(ctx, url, 1<<20)
	if err != nil {
		return Manifest{}, fmt.Errorf("manifest fetch: %w", err)
	}
	if len(pubKeys) > 0 {
		sig, err := fetchBytes(ctx, url+".sig", 4096)
		if err != nil {
			return Manifest{}, fmt.Errorf("manifest signature fetch (key configured, refusing unsigned): %w", err)
		}
		verified := false
		for _, k := range pubKeys {
			if ed25519.Verify(k, raw, normalizeSig(sig)) {
				verified = true
				break
			}
		}
		if !verified {
			return Manifest{}, fmt.Errorf("manifest signature verification FAILED for %s", url)
		}
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("manifest decode: %w", err)
	}
	if !version.Valid(m.Version) {
		return Manifest{}, fmt.Errorf("manifest version %q is not semver", m.Version)
	}
	if m.Images.Controller == "" || m.Images.Runner == "" {
		return Manifest{}, fmt.Errorf("manifest %s is missing image refs", m.Version)
	}
	return m, nil
}

// ParseManifestPublicKeys parses one or more concatenated PEM-encoded ed25519
// public keys (the CLAW_MANIFEST_PUBKEY env / updates.manifestPublicKey
// value) — a rotation ring; verification passes on any of them. "" → nil
// (unsigned mode).
func ParseManifestPublicKeys(pemStr string) ([]ed25519.PublicKey, error) {
	if strings.TrimSpace(pemStr) == "" {
		return nil, nil
	}
	var keys []ed25519.PublicKey
	rest := []byte(pemStr)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("manifest public key: %w", err)
		}
		key, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("manifest public key: %T is not ed25519", parsed)
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("manifest public key: not PEM")
	}
	return keys, nil
}

// normalizeSig accepts a raw 64-byte ed25519 signature (openssl pkeyutl
// output) or its base64 encoding.
func normalizeSig(sig []byte) []byte {
	if len(sig) == ed25519.SignatureSize {
		return sig
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil {
		return sig // let Verify fail
	}
	return decoded
}

func fetchBytes(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// Degradation evaluates whether a release can be self-applied on this install
// (DESIGN.md §24.3): requiresHelmUpgrade, a too-old supervisor, or a custom
// registry whose images the manifest can't speak for. Returns ok=false with a
// human reason for the Slack prompt.
func Degradation(cp *clawv1alpha1.ControlPlane, m Manifest, supervisorVersion string) (reason string, ok bool) {
	if m.RequiresHelmUpgrade {
		return "this release changes the chart — it requires a helm upgrade", false
	}
	if m.MinSupervisorVersion != "" && !version.Same(m.MinSupervisorVersion, supervisorVersion) &&
		version.Newer(m.MinSupervisorVersion, supervisorVersion) {
		return fmt.Sprintf("it needs supervisor %s (running %s)", m.MinSupervisorVersion, supervisorVersion), false
	}
	// A custom registry (IMAGE_REPO installs) makes the manifest's digests
	// meaningless here — unless the operator points manifestURL at their own.
	if repo := controllerRepo(cp.Spec); !strings.HasPrefix(m.Images.Controller, repo) {
		return fmt.Sprintf("the manifest's images don't come from this install's registry (%s)", repo), false
	}
	if repo := runnerRepo(cp.Spec); !strings.HasPrefix(m.Images.Runner, repo) {
		return fmt.Sprintf("the manifest's runner image doesn't come from this install's registry (%s)", repo), false
	}
	return "", true
}
