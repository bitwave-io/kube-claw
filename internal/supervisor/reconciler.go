package supervisor

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
		// or config changes roll the pod at the same version). Compare ONLY the
		// fields this reconciler owns — the apiserver defaults the rest (probe
		// timings, port protocols, termination paths), so a whole-container
		// DeepEqual never matches and would rewrite the object every reconcile.
		desired := BuildStatefulSet(&cp, des)
		if templateDrifted(&sts, desired) {
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
	// Kubernetes "forced rollback" caveat: an OrderedReady StatefulSet whose
	// update is stuck on a never-Ready pod (ImagePullBackOff, crash-before-
	// ready) does NOT recover when the template is reverted — the stuck pod
	// must be deleted so the controller recreates it at the reverted revision.
	r.deleteStuckPods(ctx, cp.Namespace)

	// Re-point the approval at the ROLLBACK TARGET rather than merely clearing
	// it: with the annotations gone, desired state would regress to the Helm
	// floor (spec.version) and immediately "upgrade" away from the version we
	// just rolled back to — a silent downgrade (caught by the k3d e2e). The
	// failed release itself stays blocked via LastRollback (the held guard),
	// and version.Newer keeps a stale rewrite from ever outranking the floor.
	if cp.Annotations == nil {
		cp.Annotations = map[string]string{}
	}
	if version.Newer(cp.Status.PreviousVersion, cp.Spec.Version) &&
		cp.Status.PreviousControllerImage != "" && cp.Status.PreviousRunnerImage != "" {
		cp.Annotations[clawv1alpha1.AnnotationApprovedVersion] = cp.Status.PreviousVersion
		cp.Annotations[clawv1alpha1.AnnotationApprovedControllerImage] = cp.Status.PreviousControllerImage
		cp.Annotations[clawv1alpha1.AnnotationApprovedRunnerImage] = cp.Status.PreviousRunnerImage
		cp.Annotations[clawv1alpha1.AnnotationApprovedBy] = "rollback"
	} else {
		// The floor already serves the rollback target (or we have no usable
		// previous refs) — the tag path covers it.
		delete(cp.Annotations, clawv1alpha1.AnnotationApprovedVersion)
		delete(cp.Annotations, clawv1alpha1.AnnotationApprovedControllerImage)
		delete(cp.Annotations, clawv1alpha1.AnnotationApprovedRunnerImage)
		delete(cp.Annotations, clawv1alpha1.AnnotationApprovedBy)
	}
	if err := r.Update(ctx, cp); err != nil {
		return ctrl.Result{}, err
	}

	lg.Info("rolling back", "from", target, "to", cp.Status.PreviousVersion)
	r.notifyAdmin(ctx, cp, fmt.Sprintf(
		":warning: kube-claw upgrade to *%s* did not confirm startup — rolling back to *%s*. I won't retry this release.",
		target, orUnknown(cp.Status.PreviousVersion)))
	return ctrl.Result{RequeueAfter: watchdogPoll}, nil
}

// templateDrifted compares only the supervisor-owned template fields against
// the live object: replicas, and the controller container's image, args, env,
// resources, and port numbers. Server-defaulted fields are ignored on purpose.
func templateDrifted(current, desired *appsv1.StatefulSet) bool {
	cur := containerByName(current, "controller")
	des := containerByName(desired, "controller")
	if cur == nil || des == nil {
		return true
	}
	if !apiequality.Semantic.DeepEqual(current.Spec.Replicas, desired.Spec.Replicas) {
		return true
	}
	if cur.Image != des.Image ||
		!apiequality.Semantic.DeepEqual(cur.Args, des.Args) ||
		!apiequality.Semantic.DeepEqual(cur.Env, des.Env) ||
		!apiequality.Semantic.DeepEqual(cur.Resources, des.Resources) {
		return true
	}
	if len(cur.Ports) != len(des.Ports) {
		return true
	}
	for i := range des.Ports {
		if cur.Ports[i].Name != des.Ports[i].Name || cur.Ports[i].ContainerPort != des.Ports[i].ContainerPort {
			return true
		}
	}
	return false
}

func containerByName(sts *appsv1.StatefulSet, name string) *corev1.Container {
	for i := range sts.Spec.Template.Spec.Containers {
		if sts.Spec.Template.Spec.Containers[i].Name == name {
			return &sts.Spec.Template.Spec.Containers[i]
		}
	}
	return nil
}

// deleteStuckPods best-effort deletes non-Ready controller pods after a
// rollback template revert (see the forced-rollback note at the call site).
func (r *Reconciler) deleteStuckPods(ctx context.Context, namespace string) {
	lg := logf.FromContext(ctx)
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(namespace),
		client.MatchingLabels{"app.kubernetes.io/name": appLabel}); err != nil {
		lg.Error(err, "list controller pods for stuck-pod cleanup")
		return
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if podReady(p) {
			continue
		}
		if err := r.Delete(ctx, p); err != nil && !apierrors.IsNotFound(err) {
			lg.Error(err, "delete stuck controller pod", "pod", p.Name)
		} else {
			lg.Info("deleted stuck controller pod so the revert can proceed", "pod", p.Name)
		}
	}
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
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
