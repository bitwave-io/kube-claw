package supervisor

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/version"
)

// Notifier delivers a failure/degradation message to the upgrade admin. The
// supervisor's implementation is one bare Slack chat.postMessage (notify.go);
// nil = log only.
type Notifier interface {
	Notify(ctx context.Context, admin, text string) error
}

// watchdogPoll is how often an in-flight update/rollback is re-checked.
const watchdogPoll = 15 * time.Second

// Reconciler reconciles a ControlPlane into the controller StatefulSet and
// runs the update watchdog (DESIGN.md §24.5).
type Reconciler struct {
	client.Client
	Notify Notifier
	// Now is injectable for watchdog tests; defaults to time.Now.
	Now func() time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&clawv1alpha1.ControlPlane{}).
		Owns(&appsv1.StatefulSet{}).
		Complete(r)
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := logf.FromContext(ctx)

	var cp clawv1alpha1.ControlPlane
	if err := r.Get(ctx, req.NamespacedName, &cp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	des := DesiredState(&cp)

	// A version that just auto-rolled-back is held — never re-applied until a
	// different version becomes desired (or a human intervenes). Without this,
	// the reconcile loop would immediately re-apply the broken release.
	held := cp.Status.LastRollback != nil && sameVersion(cp.Status.LastRollback.From, des.Version)

	var sts appsv1.StatefulSet
	err := r.Get(ctx, types.NamespacedName{Namespace: cp.Namespace, Name: StatefulSetName}, &sts)
	switch {
	case apierrors.IsNotFound(err):
		// First install (or ≤0.3.x adoption after Helm deleted its StatefulSet):
		// create it. The fixed name/serviceName reattaches the existing data PVC
		// (data-claw-controller-0) — DESIGN.md §24.8.
		desired := BuildStatefulSet(&cp, des)
		if err := ctrl.SetControllerReference(&cp, desired, r.Scheme()); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, desired); err != nil {
			return ctrl.Result{}, err
		}
		lg.Info("created controller statefulset", "version", des.Version, "image", des.ControllerImage)
		cp.Status.RunningControllerImage = des.ControllerImage
		cp.Status.RunningRunnerImage = des.RunnerImage
		if cp.Status.Phase == "" {
			cp.Status.Phase = clawv1alpha1.PhaseIdle
		}
		return ctrl.Result{}, r.Status().Update(ctx, &cp)
	case err != nil:
		return ctrl.Result{}, err
	}

	applied := currentImage(&sts)
	switch {
	case applied != des.ControllerImage && !held:
		return r.beginUpdate(ctx, &cp, &sts, des)
	case applied != des.ControllerImage && held:
		// Desired is the rolled-back version: hold position, keep watching the
		// rollback/degraded state below.
		lg.Info("holding rolled-back version", "held", des.Version)
	default:
		// Images match; keep the rest of the pod template converged (resource
		// or config changes roll the pod at the same version).
		desired := BuildStatefulSet(&cp, des)
		if !apiequality.Semantic.DeepEqual(sts.Spec.Template.Spec.Containers, desired.Spec.Template.Spec.Containers) ||
			!apiequality.Semantic.DeepEqual(sts.Spec.Replicas, desired.Spec.Replicas) {
			sts.Spec.Template = desired.Spec.Template
			sts.Spec.Replicas = desired.Spec.Replicas
			if err := r.Update(ctx, &sts); err != nil {
				return ctrl.Result{}, err
			}
			lg.Info("converged controller pod template", "version", des.Version)
		}
	}

	return r.watchdog(ctx, &cp, des)
}

// beginUpdate records the rollback point and applies the new images
// (DESIGN.md §24.5 steps 1-2).
func (r *Reconciler) beginUpdate(ctx context.Context, cp *clawv1alpha1.ControlPlane, sts *appsv1.StatefulSet, des Desired) (ctrl.Result, error) {
	lg := logf.FromContext(ctx)
	now := metav1.NewTime(r.now())

	prevCtrl, prevRun := cp.Status.RunningControllerImage, cp.Status.RunningRunnerImage
	if prevCtrl == "" {
		prevCtrl = currentImage(sts)
	}
	if prevRun == "" {
		prevRun = currentRunnerImage(sts)
	}

	cp.Status.PreviousControllerImage = prevCtrl
	cp.Status.PreviousRunnerImage = prevRun
	cp.Status.PreviousVersion = cp.Status.RunningVersion
	cp.Status.UpdateTarget = des.Version
	cp.Status.UpdateStartedAt = &now
	// The migration flag is only known for manifest-announced releases; a
	// Helm-driven version change is treated as migration-free (rollbackable).
	cp.Status.UpdateContainsMigration = des.FromApproval &&
		version.Same(des.Version, cp.Status.AvailableVersion) && cp.Status.AvailableContainsMigration
	cp.Status.Phase = clawv1alpha1.PhaseUpdating
	cp.Status.RunningControllerImage = des.ControllerImage
	cp.Status.RunningRunnerImage = des.RunnerImage
	if err := r.Status().Update(ctx, cp); err != nil {
		return ctrl.Result{}, err
	}

	desired := BuildStatefulSet(cp, des)
	sts.Spec.Template = desired.Spec.Template
	sts.Spec.Replicas = desired.Spec.Replicas
	if err := r.Update(ctx, sts); err != nil {
		return ctrl.Result{}, err
	}
	lg.Info("applying update", "target", des.Version, "controllerImage", des.ControllerImage,
		"previous", prevCtrl, "containsMigration", cp.Status.UpdateContainsMigration)
	return ctrl.Result{RequeueAfter: watchdogPoll}, nil
}

// sameVersion is semver equality with a string-equality fallback for
// non-semver versions (dev/k3d installs pin version "dev" — a helm-driven
// image change there must still be confirmable, or the watchdog would roll
// back a healthy deploy).
func sameVersion(a, b string) bool {
	return version.Same(a, b) || (a != "" && a == b)
}

// watchdog drives the Updating/RollingBack/Degraded lifecycle: success is the
// controller writing status.runningVersion (startup-confirmed, §24.5), not
// pod-Ready; the confirm deadline triggers rollback (or Degraded-hold for
// migration releases).
func (r *Reconciler) watchdog(ctx context.Context, cp *clawv1alpha1.ControlPlane, des Desired) (ctrl.Result, error) {
	lg := logf.FromContext(ctx)

	switch cp.Status.Phase {
	case clawv1alpha1.PhaseUpdating:
		if sameVersion(cp.Status.RunningVersion, cp.Status.UpdateTarget) {
			cp.Status.Phase = clawv1alpha1.PhaseIdle
			cp.Status.UpdateTarget = ""
			cp.Status.UpdateStartedAt = nil
			cp.Status.UpdateContainsMigration = false
			lg.Info("update confirmed", "version", cp.Status.RunningVersion)
			return ctrl.Result{}, r.Status().Update(ctx, cp)
		}
		if !r.pastDeadline(cp, cp.Status.UpdateStartedAt) {
			return ctrl.Result{RequeueAfter: watchdogPoll}, nil
		}
		return r.failUpdate(ctx, cp)

	case clawv1alpha1.PhaseRollingBack:
		if sameVersion(cp.Status.RunningVersion, cp.Status.PreviousVersion) {
			cp.Status.Phase = clawv1alpha1.PhaseIdle
			cp.Status.UpdateTarget = ""
			cp.Status.UpdateStartedAt = nil
			lg.Info("rollback confirmed", "version", cp.Status.RunningVersion)
			return ctrl.Result{}, r.Status().Update(ctx, cp)
		}
		if !r.pastDeadline(cp, cp.Status.UpdateStartedAt) {
			return ctrl.Result{RequeueAfter: watchdogPoll}, nil
		}
		// Rollback itself failed to confirm — nothing left to try automatically.
		cp.Status.Phase = clawv1alpha1.PhaseDegraded
		r.notifyAdmin(ctx, cp, fmt.Sprintf(
			":rotating_light: kube-claw rollback to *%s* did not confirm — the control plane needs a human (`kubectl -n %s get pods`).",
			cp.Status.PreviousVersion, cp.Namespace))
		return ctrl.Result{}, r.Status().Update(ctx, cp)

	case clawv1alpha1.PhaseDegraded:
		// Recovery detection: a human fixed it (or the stuck version finally
		// confirmed) — the controller's runningVersion matches desired again.
		if sameVersion(cp.Status.RunningVersion, des.Version) {
			cp.Status.Phase = clawv1alpha1.PhaseIdle
			cp.Status.UpdateTarget = ""
			cp.Status.UpdateStartedAt = nil
			cp.Status.UpdateContainsMigration = false
			lg.Info("recovered from degraded", "version", cp.Status.RunningVersion)
			return ctrl.Result{}, r.Status().Update(ctx, cp)
		}
		return ctrl.Result{RequeueAfter: watchdogPoll}, nil
	}
	return ctrl.Result{}, nil
}

// failUpdate handles a confirm-deadline miss: auto-rollback for ordinary
// releases, Degraded-hold for migration releases (old code on a new schema is
// worse than staying down — §24.5).
func (r *Reconciler) failUpdate(ctx context.Context, cp *clawv1alpha1.ControlPlane) (ctrl.Result, error) {
	lg := logf.FromContext(ctx)
	target := cp.Status.UpdateTarget

	if cp.Status.UpdateContainsMigration {
		cp.Status.Phase = clawv1alpha1.PhaseDegraded
		lg.Info("update failed; migration release — holding degraded, not rolling back", "target", target)
		r.notifyAdmin(ctx, cp, fmt.Sprintf(
			":rotating_light: kube-claw upgrade to *%s* did not confirm in time. This release migrates the database, so I did NOT auto-roll-back — restore manually (a pre-migration snapshot is at claw.db.pre-%s on the data volume).",
			target, target))
		return ctrl.Result{}, r.Status().Update(ctx, cp)
	}

	if cp.Status.PreviousControllerImage == "" {
		cp.Status.Phase = clawv1alpha1.PhaseDegraded
		lg.Info("update failed; no previous image to roll back to", "target", target)
		r.notifyAdmin(ctx, cp, fmt.Sprintf(
			":rotating_light: kube-claw *%s* did not confirm startup and there is no previous version to roll back to.", target))
		return ctrl.Result{}, r.Status().Update(ctx, cp)
	}

	now := metav1.NewTime(r.now())
	cp.Status.Phase = clawv1alpha1.PhaseRollingBack
	cp.Status.LastRollback = &clawv1alpha1.RollbackRecord{
		From:   target,
		To:     cp.Status.PreviousVersion,
		Reason: "startup-confirm deadline exceeded",
		At:     now,
	}
	// Rollback is digest-pinned to the RECORDED previous images (§24.5 step 4).
	cp.Status.RunningControllerImage = cp.Status.PreviousControllerImage
	cp.Status.RunningRunnerImage = cp.Status.PreviousRunnerImage
	cp.Status.UpdateStartedAt = &now // restart the confirm clock for the rollback
	if err := r.Status().Update(ctx, cp); err != nil {
		return ctrl.Result{}, err
	}

	var sts appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{Namespace: cp.Namespace, Name: StatefulSetName}, &sts); err != nil {
		return ctrl.Result{}, err
	}
	setImages(&sts, cp.Status.PreviousControllerImage, cp.Status.PreviousRunnerImage)
	if err := r.Update(ctx, &sts); err != nil {
		return ctrl.Result{}, err
	}

	// A failed approval must not re-apply on the next reconcile: clear it.
	if cp.Annotations[clawv1alpha1.AnnotationApprovedVersion] == target {
		delete(cp.Annotations, clawv1alpha1.AnnotationApprovedVersion)
		delete(cp.Annotations, clawv1alpha1.AnnotationApprovedControllerImage)
		delete(cp.Annotations, clawv1alpha1.AnnotationApprovedRunnerImage)
		delete(cp.Annotations, clawv1alpha1.AnnotationApprovedBy)
		if err := r.Update(ctx, cp); err != nil {
			return ctrl.Result{}, err
		}
	}

	lg.Info("rolling back", "from", target, "to", cp.Status.PreviousVersion)
	r.notifyAdmin(ctx, cp, fmt.Sprintf(
		":warning: kube-claw upgrade to *%s* did not confirm startup — rolling back to *%s*. I won't retry this release.",
		target, orUnknown(cp.Status.PreviousVersion)))
	return ctrl.Result{RequeueAfter: watchdogPoll}, nil
}

// pastDeadline reports whether the confirm deadline for an in-flight update
// has passed. A missing start time counts as past (defensive).
func (r *Reconciler) pastDeadline(cp *clawv1alpha1.ControlPlane, started *metav1.Time) bool {
	if started == nil {
		return true
	}
	deadline := 10 * time.Minute
	if d, err := time.ParseDuration(cp.Spec.Updates.ConfirmDeadline); err == nil && d > 0 {
		deadline = d
	}
	return r.now().After(started.Add(deadline))
}

// notifyAdmin best-effort delivers a failure message to the upgrade admin
// (mirrored onto the CR by the controller — the supervisor has no store).
func (r *Reconciler) notifyAdmin(ctx context.Context, cp *clawv1alpha1.ControlPlane, text string) {
	admin := cp.Annotations[clawv1alpha1.AnnotationUpgradeAdmin]
	if r.Notify == nil || admin == "" {
		logf.FromContext(ctx).Info("upgrade notification (no notifier/admin)", "text", text)
		return
	}
	if err := r.Notify.Notify(ctx, admin, text); err != nil {
		logf.FromContext(ctx).Error(err, "notify upgrade admin")
	}
}

// currentImage returns the controller container's image ("" if absent).
func currentImage(sts *appsv1.StatefulSet) string {
	for i := range sts.Spec.Template.Spec.Containers {
		if sts.Spec.Template.Spec.Containers[i].Name == "controller" {
			return sts.Spec.Template.Spec.Containers[i].Image
		}
	}
	return ""
}

// currentRunnerImage extracts the --runner-image arg from the controller
// container (the runner ref rides along as an argument, not a container).
func currentRunnerImage(sts *appsv1.StatefulSet) string {
	const prefix = "--runner-image="
	for i := range sts.Spec.Template.Spec.Containers {
		c := &sts.Spec.Template.Spec.Containers[i]
		if c.Name != "controller" {
			continue
		}
		for _, a := range c.Args {
			if len(a) > len(prefix) && a[:len(prefix)] == prefix {
				return a[len(prefix):]
			}
		}
	}
	return ""
}

// setImages rewrites the controller image + --runner-image arg in one patch so
// the pair moves in lockstep (§24.5 step 2).
func setImages(sts *appsv1.StatefulSet, controllerImage, runnerImage string) {
	const prefix = "--runner-image="
	for i := range sts.Spec.Template.Spec.Containers {
		c := &sts.Spec.Template.Spec.Containers[i]
		if c.Name != "controller" {
			continue
		}
		c.Image = controllerImage
		for j, a := range c.Args {
			if len(a) > len(prefix) && a[:len(prefix)] == prefix {
				c.Args[j] = prefix + runnerImage
			}
		}
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}
