package supervisor

import (
	"context"
	"crypto/ed25519"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/version"
)

// Poller periodically fetches the release manifest and records what's
// available on ControlPlane status (DESIGN.md §24.3). Detection runs in every
// mode; in auto mode the poller also self-approves applicable releases
// (digest-pinned, still health-watched). The Slack conversation is the
// CONTROLLER's job — the poller only writes state.
type Poller struct {
	client.Client
	Namespace       string
	DefaultInterval time.Duration
	// PubKeys verify the manifest's detached signature (T-9, any-of — a
	// rotation ring). nil = unsigned
	// mode; set = fail closed on a missing/invalid signature.
	PubKeys []ed25519.PublicKey
	// Fetch is injectable for tests; defaults to FetchManifestSigned with PubKeys.
	Fetch func(ctx context.Context, url string) (Manifest, error)
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
	// Kick wakes the poll loop immediately (an on-demand release check — the
	// reconciler pokes it when it sees the check-requested annotation). The
	// minute tick is the fallback if a poke is ever missed.
	Kick chan struct{}
}

// Poke requests an immediate poll (non-blocking; coalesces).
func (p *Poller) Poke() {
	if p.Kick == nil {
		return
	}
	select {
	case p.Kick <- struct{}{}:
	default:
	}
}

// NeedLeaderElection lets the poller run on the (single) supervisor replica.
func (p *Poller) NeedLeaderElection() bool { return false }

// Start ticks once a minute and polls each ControlPlane whose checkInterval
// has elapsed (manager.Runnable).
func (p *Poller) Start(ctx context.Context) error {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	// One immediate pass so a fresh install learns about releases without
	// waiting a full interval.
	p.pollAll(ctx)
	kick := p.Kick
	if kick == nil {
		kick = make(chan struct{}) // never fires
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			p.pollAll(ctx)
		case <-kick:
			p.pollAll(ctx)
		}
	}
}

func (p *Poller) pollAll(ctx context.Context) {
	lg := logf.Log.WithName("release-poller")
	var list clawv1alpha1.ControlPlaneList
	if err := p.List(ctx, &list, client.InNamespace(p.Namespace)); err != nil {
		lg.Error(err, "list controlplanes")
		return
	}
	for i := range list.Items {
		cp := &list.Items[i]
		requested := cp.Annotations[clawv1alpha1.AnnotationCheckRequested] != ""
		if !requested && !p.due(cp) {
			continue
		}
		if requested {
			// Consume the request BEFORE polling: a failing fetch must not
			// leave the annotation re-triggering every minute forever.
			p.clearCheckRequested(ctx, cp)
		}
		if err := p.pollOne(ctx, cp); err != nil {
			lg.Error(err, "poll release manifest", "controlplane", cp.Name)
		}
	}
}

// clearCheckRequested best-effort removes the on-demand-check annotation.
func (p *Poller) clearCheckRequested(ctx context.Context, cp *clawv1alpha1.ControlPlane) {
	fresh := cp.DeepCopy()
	delete(fresh.Annotations, clawv1alpha1.AnnotationCheckRequested)
	if err := p.Update(ctx, fresh); err != nil {
		logf.Log.WithName("release-poller").Error(err, "clear check-requested annotation", "controlplane", cp.Name)
	}
}

// due reports whether this CR's checkInterval has elapsed since LastCheckTime.
func (p *Poller) due(cp *clawv1alpha1.ControlPlane) bool {
	interval := p.DefaultInterval
	if interval <= 0 {
		interval = time.Hour
	}
	if d, err := time.ParseDuration(cp.Spec.Updates.CheckInterval); err == nil && d > 0 {
		interval = d
	}
	if cp.Status.LastCheckTime == nil {
		return true
	}
	return p.now().After(cp.Status.LastCheckTime.Add(interval))
}

func (p *Poller) pollOne(ctx context.Context, cp *clawv1alpha1.ControlPlane) error {
	lg := logf.Log.WithName("release-poller")
	fetch := p.Fetch
	if fetch == nil {
		fetch = func(ctx context.Context, url string) (Manifest, error) {
			return FetchManifestSigned(ctx, url, p.PubKeys)
		}
	}
	url := cp.Spec.Updates.ManifestURL
	if url == "" {
		url = DefaultManifestURL(cp.Spec.Updates.Channel)
	}
	m, err := fetch(ctx, url)
	now := metav1.NewTime(p.now())
	if err != nil {
		// Record the attempt so a broken URL doesn't hot-loop, keep prior state.
		cp.Status.LastCheckTime = &now
		_ = p.Status().Update(ctx, cp)
		return err
	}

	reason, applicable := Degradation(cp, m, version.Get())
	cp.Status.AvailableVersion = m.Version
	cp.Status.AvailableNotes = m.Notes
	cp.Status.AvailableContainsMigration = m.ContainsMigration
	cp.Status.AvailableControllerImage = m.Images.Controller
	cp.Status.AvailableRunnerImage = m.Images.Runner
	cp.Status.AvailableRequiresHelm = !applicable
	cp.Status.AvailableRequiresHelmReason = reason
	cp.Status.LastCheckTime = &now
	if err := p.Status().Update(ctx, cp); err != nil {
		return err
	}
	lg.Info("release check", "available", m.Version, "selfApplicable", applicable, "reason", reason)

	// Auto mode: self-approve an applicable, strictly-newer release — unless it
	// already failed here (rollback hold) or is already approved.
	if cp.Spec.Updates.Mode != clawv1alpha1.UpdateModeAuto || !applicable {
		return nil
	}
	current := version.Max(cp.Spec.Version, cp.Status.RunningVersion)
	if !version.Newer(m.Version, current) {
		return nil
	}
	if cp.Status.LastRollback != nil && version.Same(cp.Status.LastRollback.From, m.Version) {
		return nil
	}
	if version.Same(cp.Annotations[clawv1alpha1.AnnotationApprovedVersion], m.Version) {
		return nil
	}
	if cp.Annotations == nil {
		cp.Annotations = map[string]string{}
	}
	cp.Annotations[clawv1alpha1.AnnotationApprovedVersion] = m.Version
	cp.Annotations[clawv1alpha1.AnnotationApprovedControllerImage] = m.Images.Controller
	cp.Annotations[clawv1alpha1.AnnotationApprovedRunnerImage] = m.Images.Runner
	cp.Annotations[clawv1alpha1.AnnotationApprovedBy] = "auto"
	if err := p.Update(ctx, cp); err != nil {
		return err
	}
	lg.Info("auto mode: approved release", "version", m.Version)
	return nil
}

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}
