// Package upgrade is the CONTROLLER's half of the self-update plane
// (DESIGN.md §24): it owns the human conversation and the store-backed policy
// state, while the supervisor (internal/supervisor) owns the machinery.
//
//   - At boot it writes status.runningVersion on the ControlPlane — the
//     startup-confirmed signal the supervisor's watchdog waits on — and
//     announces version changes ("upgraded ✅") to the upgrade admin.
//   - It watches what the supervisor's poller found (status.available*) and
//     prompts the admin in Slack (mode=prompt), or just announces
//     (manual / requiresHelmUpgrade releases).
//   - It applies the admin's decision (Slack buttons or CLI break-glass) by
//     writing the digest-pinned approval annotations the supervisor consumes.
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/version"
)

// Poster is the Slack surface the coordinator needs (satisfied by
// slackrouter.Notifier); nil when Slack is off.
type Poster interface {
	PostUpgradePrompt(ctx context.Context, admin, current, available, notes, reason string, containsMigration, canApply bool) error
	PostReply(ctx context.Context, channel, threadTS, text string) error
}

// Coordinator runs in the controller. It is also the slackrouter.UpgradeActor
// behind the Approve/Skip/Later buttons.
type Coordinator struct {
	Store     store.Store
	Reader    client.Reader // uncached CR reads
	Writer    client.Client // CR writes (annotations + status)
	Notifier  Poster
	Name      string // ControlPlane name (CLAW_CONTROLPLANE_NAME)
	Namespace string // ControlPlane namespace (CLAW_CONTROLPLANE_NAMESPACE)
	Interval  time.Duration
	// RunningVersion is the stamped build version (injectable for tests).
	RunningVersion string
}

// NeedLeaderElection — single controller replica; run unconditionally.
func (c *Coordinator) NeedLeaderElection() bool { return false }

func (c *Coordinator) interval() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return 30 * time.Second
}

func (c *Coordinator) runningVersion() string {
	if c.RunningVersion != "" {
		return c.RunningVersion
	}
	return version.Get()
}

// Start confirms startup, then ticks the prompt/mirror loop (manager.Runnable).
func (c *Coordinator) Start(ctx context.Context) error {
	lg := logf.Log.WithName("upgrade")
	// Startup confirmation retries until the CR is reachable — this write is
	// what tells the supervisor's watchdog the new version is alive, so it must
	// not be given up on transient errors.
	for {
		if err := c.ConfirmStartup(ctx); err == nil {
			break
		} else {
			lg.Error(err, "confirm startup on ControlPlane (will retry)")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(10 * time.Second):
		}
	}

	tick := time.NewTicker(c.interval())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := c.Tick(ctx); err != nil {
				lg.Error(err, "upgrade tick")
			}
		}
	}
}

// ConfirmStartup writes status.runningVersion (the startup-confirmed signal,
// §24.5) and announces a version change to the admin. Called once the manager
// is up — i.e. after migrations ran and the store opened.
func (c *Coordinator) ConfirmStartup(ctx context.Context) error {
	lg := logf.Log.WithName("upgrade")
	var was string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cp clawv1alpha1.ControlPlane
		if err := c.Reader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Name}, &cp); err != nil {
			return err
		}
		was = cp.Status.RunningVersion
		if was == c.runningVersion() {
			return nil
		}
		cp.Status.RunningVersion = c.runningVersion()
		return c.Writer.Status().Update(ctx, &cp)
	})
	if err != nil {
		return err
	}
	lg.Info("confirmed startup on ControlPlane", "version", c.runningVersion(), "was", was)
	if was != "" && was != c.runningVersion() {
		c.announce(ctx, fmt.Sprintf("✅ kube-claw is now running *%s* (was %s).", c.runningVersion(), was))
	}
	return nil
}

// Tick mirrors the admin setting onto the CR and prompts/announces newly
// available releases.
func (c *Coordinator) Tick(ctx context.Context) error {
	var cp clawv1alpha1.ControlPlane
	if err := c.Reader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Name}, &cp); err != nil {
		return err
	}
	if err := c.mirrorAdmin(ctx, &cp); err != nil {
		return err
	}
	return c.maybePrompt(ctx, &cp)
}

// mirrorAdmin copies the store's upgrade-admin and management-channel settings
// onto CR annotations so the supervisor (no store access) can deliver rollback
// failures (§24.6).
func (c *Coordinator) mirrorAdmin(ctx context.Context, cp *clawv1alpha1.ControlPlane) error {
	admin, _ := c.getSetting(ctx, store.SettingUpgradeAdmin)
	mgmt, _ := c.getSetting(ctx, store.SettingMgmtChannel)
	if cp.Annotations[clawv1alpha1.AnnotationUpgradeAdmin] == admin &&
		cp.Annotations[clawv1alpha1.AnnotationMgmtChannel] == mgmt {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh clawv1alpha1.ControlPlane
		if err := c.Reader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Name}, &fresh); err != nil {
			return err
		}
		if fresh.Annotations == nil {
			fresh.Annotations = map[string]string{}
		}
		fresh.Annotations[clawv1alpha1.AnnotationUpgradeAdmin] = admin
		fresh.Annotations[clawv1alpha1.AnnotationMgmtChannel] = mgmt
		return c.Writer.Update(ctx, &fresh)
	})
}

// maybePrompt decides whether the available release warrants a Slack message
// (§24.4): buttons in prompt mode, notify-only for manual mode and
// requiresHelmUpgrade releases, silence for skipped/deferred/known versions.
func (c *Coordinator) maybePrompt(ctx context.Context, cp *clawv1alpha1.ControlPlane) error {
	avail := cp.Status.AvailableVersion
	if avail == "" || c.Notifier == nil {
		return nil
	}
	current := version.Max(cp.Spec.Version, cp.Status.RunningVersion)
	if !version.Newer(avail, current) {
		return nil
	}
	if skipped, _ := c.getSetting(ctx, store.SettingSkippedVersion); version.Same(skipped, avail) {
		return nil
	}
	if notified, _ := c.getSetting(ctx, store.SettingNotifiedVersion); version.Same(notified, avail) {
		return nil
	}
	if remind, _ := c.getSetting(ctx, store.SettingRemindAfter); remind != "" {
		if t, err := time.Parse(time.RFC3339, remind); err == nil && time.Now().Before(t) {
			return nil
		}
	}
	if version.Same(cp.Annotations[clawv1alpha1.AnnotationApprovedVersion], avail) {
		return nil // already approved; the supervisor is (or will be) applying it
	}
	if cp.Status.LastRollback != nil && version.Same(cp.Status.LastRollback.From, avail) {
		return nil // failed here before; never re-offer automatically
	}
	admin, err := c.getSetting(ctx, store.SettingUpgradeAdmin)
	if err != nil {
		return nil
	}
	mgmt, _ := c.getSetting(ctx, store.SettingMgmtChannel)
	if admin == "" && mgmt == "" {
		// Nobody to tell: surfaced via /ui/settings + CLI. Deliberately does NOT
		// burn the notified marker — the prompt fires once a target appears.
		return nil
	}

	canApply := cp.Spec.Updates.Mode == clawv1alpha1.UpdateModePrompt && !cp.Status.AvailableRequiresHelm
	reason := cp.Status.AvailableRequiresHelmReason
	if cp.Spec.Updates.Mode == clawv1alpha1.UpdateModeManual {
		canApply, reason = false, "this install is helm-managed (updates.mode=manual)"
	}
	if cp.Spec.Updates.Mode == clawv1alpha1.UpdateModeAuto && !cp.Status.AvailableRequiresHelm {
		return nil // auto mode: the poller self-approves; the boot announce covers it
	}
	// FYI to the management channel (no buttons — approval stays personal).
	if mgmt != "" {
		text := fmt.Sprintf(":package: kube-claw *%s* is available (running %s).", avail, orDev(cp.Status.RunningVersion))
		if cp.Status.AvailableNotes != "" {
			text += "\n> " + cp.Status.AvailableNotes
		}
		switch {
		case reason != "":
			text += "\n_" + reason + "_"
		case admin != "":
			text += fmt.Sprintf("\nI've asked <@%s> to approve.", admin)
		default:
			text += "\nNo upgrade admin is claimed — set one (`claw settings set upgrade-admin U…`) to approve from Slack."
		}
		if err := c.Notifier.PostReply(ctx, mgmt, "", text); err != nil {
			logf.Log.WithName("upgrade").Error(err, "announce release to management channel")
		}
	}
	if admin != "" {
		if err := c.Notifier.PostUpgradePrompt(ctx, admin, orDev(cp.Status.RunningVersion), avail,
			cp.Status.AvailableNotes, reason, cp.Status.AvailableContainsMigration, canApply); err != nil {
			return err
		}
		logf.Log.WithName("upgrade").Info("posted upgrade prompt", "available", avail, "canApply", canApply)
	}
	return c.setSetting(ctx, store.SettingNotifiedVersion, avail)
}

// --- slackrouter.UpgradeActor + CLI break-glass -----------------------------

// ErrNotOffered is returned when the approved version isn't the offered one.
var ErrNotOffered = errors.New("that version is not the currently offered release")

// Approve writes the digest-pinned approval annotations (§24.2). The image
// refs are copied from status.available* — captured at approval time, so what
// the admin approved is exactly what runs.
func (c *Coordinator) Approve(ctx context.Context, ver, byUser string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cp clawv1alpha1.ControlPlane
		if err := c.Reader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Name}, &cp); err != nil {
			return err
		}
		if !version.Same(cp.Status.AvailableVersion, ver) {
			return fmt.Errorf("%w (offered: %s)", ErrNotOffered, orNone(cp.Status.AvailableVersion))
		}
		if cp.Status.AvailableRequiresHelm {
			return fmt.Errorf("release %s can't be self-applied: %s", ver, cp.Status.AvailableRequiresHelmReason)
		}
		if cp.Spec.Updates.Mode == clawv1alpha1.UpdateModeManual {
			return errors.New("this install is helm-managed (updates.mode=manual)")
		}
		if cp.Annotations == nil {
			cp.Annotations = map[string]string{}
		}
		cp.Annotations[clawv1alpha1.AnnotationApprovedVersion] = cp.Status.AvailableVersion
		cp.Annotations[clawv1alpha1.AnnotationApprovedControllerImage] = cp.Status.AvailableControllerImage
		cp.Annotations[clawv1alpha1.AnnotationApprovedRunnerImage] = cp.Status.AvailableRunnerImage
		cp.Annotations[clawv1alpha1.AnnotationApprovedBy] = byUser
		return c.Writer.Update(ctx, &cp)
	})
}

// Skip marks a version never-re-prompted.
func (c *Coordinator) Skip(ctx context.Context, ver, _ string) error {
	return c.setSetting(ctx, store.SettingSkippedVersion, ver)
}

// Later suppresses the prompt for one check interval, then re-arms it.
func (c *Coordinator) Later(ctx context.Context, ver string) error {
	interval := 6 * time.Hour
	var cp clawv1alpha1.ControlPlane
	if err := c.Reader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Name}, &cp); err == nil {
		if d, err := time.ParseDuration(cp.Spec.Updates.CheckInterval); err == nil && d > 0 {
			interval = d
		}
	}
	if err := c.setSetting(ctx, store.SettingRemindAfter, time.Now().Add(interval).UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	// Clear the notified marker so the prompt re-fires after the remind window.
	return c.setSetting(ctx, store.SettingNotifiedVersion, "")
}

// Status returns a JSON-friendly view for the CLI (`claw upgrade status`).
func (c *Coordinator) Status(ctx context.Context) (map[string]any, error) {
	var cp clawv1alpha1.ControlPlane
	if err := c.Reader.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Name}, &cp); err != nil {
		return nil, err
	}
	out := map[string]any{
		"mode":             cp.Spec.Updates.Mode,
		"helmPinned":       cp.Spec.Version,
		"runningVersion":   orNone(cp.Status.RunningVersion),
		"availableVersion": orNone(cp.Status.AvailableVersion),
		"phase":            orNone(cp.Status.Phase),
	}
	if cp.Status.AvailableRequiresHelm {
		out["requiresHelmUpgrade"] = cp.Status.AvailableRequiresHelmReason
	}
	if v := cp.Annotations[clawv1alpha1.AnnotationApprovedVersion]; v != "" {
		out["approvedVersion"] = v
		out["approvedBy"] = cp.Annotations[clawv1alpha1.AnnotationApprovedBy]
	}
	if lr := cp.Status.LastRollback; lr != nil {
		out["lastRollback"] = map[string]string{"from": lr.From, "to": lr.To, "reason": lr.Reason, "at": lr.At.UTC().Format(time.RFC3339)}
	}
	if admin, _ := c.getSetting(ctx, store.SettingUpgradeAdmin); admin != "" {
		out["upgradeAdmin"] = admin
	}
	return out, nil
}

// announce best-effort delivers lifecycle news to the upgrade admin (DM) and
// the management channel, when either is configured.
func (c *Coordinator) announce(ctx context.Context, text string) {
	if c.Notifier == nil {
		return
	}
	admin, _ := c.getSetting(ctx, store.SettingUpgradeAdmin)
	mgmt, _ := c.getSetting(ctx, store.SettingMgmtChannel)
	for _, target := range []string{admin, mgmt} {
		if target == "" {
			continue
		}
		if err := c.Notifier.PostReply(ctx, target, "", text); err != nil {
			logf.Log.WithName("upgrade").Error(err, "announce", "target", target)
		}
	}
}

func (c *Coordinator) getSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := c.Store.Tx(ctx, func(tx store.Tx) error {
		got, err := tx.GetSetting(key)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		v = got
		return err
	})
	return v, err
}

func (c *Coordinator) setSetting(ctx context.Context, key, value string) error {
	return c.Store.Tx(ctx, func(tx store.Tx) error { return tx.SetSetting(key, value) })
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func orDev(s string) string {
	if s == "" {
		return "dev"
	}
	return s
}
