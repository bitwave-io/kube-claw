package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/version"
)

// Manifest is the published release manifest (DESIGN.md §24.3): one HTTPS GET,
// digest-pinned image refs, and the flags that gate self-application. Signing
// is deferred (T-9); the schema reserves room by versioning itself.
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
	return fmt.Sprintf("https://github.com/traego/kube-claw/releases/latest/download/manifest-%s.json", channel)
}

// FetchManifest GETs and validates a release manifest.
func FetchManifest(ctx context.Context, url string) (Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Manifest{}, err
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return Manifest{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Manifest{}, fmt.Errorf("manifest fetch: %s", resp.Status)
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
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
