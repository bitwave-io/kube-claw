package supervisor

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := clawv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func testCP(mode string) *clawv1alpha1.ControlPlane {
	return &clawv1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "claw", Namespace: "claw-system"},
		Spec: clawv1alpha1.ControlPlaneSpec{
			Version: "0.4.0",
			Updates: clawv1alpha1.UpdatesSpec{Mode: mode, ConfirmDeadline: "10m"},
		},
	}
}

func TestDesiredState(t *testing.T) {
	base := testCP(clawv1alpha1.UpdateModePrompt)

	// No approval → helm tag path.
	d := DesiredState(base)
	if d.Version != "0.4.0" || d.FromApproval ||
		d.ControllerImage != "docker.io/bitwavecode/kube-claw-controller:0.4.0" ||
		d.RunnerImage != "docker.io/bitwavecode/kube-claw-runner:0.4.0" {
		t.Fatalf("no-approval desired = %+v", d)
	}

	// Newer approval with digest-pinned refs wins.
	appr := base.DeepCopy()
	appr.Annotations = map[string]string{
		clawv1alpha1.AnnotationApprovedVersion:         "v0.5.0",
		clawv1alpha1.AnnotationApprovedControllerImage: "docker.io/bitwavecode/kube-claw-controller@sha256:aaa",
		clawv1alpha1.AnnotationApprovedRunnerImage:     "docker.io/bitwavecode/kube-claw-runner@sha256:bbb",
	}
	d = DesiredState(appr)
	if !d.FromApproval || d.Version != "v0.5.0" || !strings.Contains(d.ControllerImage, "@sha256:aaa") {
		t.Fatalf("approval desired = %+v", d)
	}

	// Helm moved past the approval (0.6.0 > 0.5.0) → helm wins (max rule).
	newer := appr.DeepCopy()
	newer.Spec.Version = "0.6.0"
	if d = DesiredState(newer); d.FromApproval || d.Version != "0.6.0" {
		t.Fatalf("helm-past-approval desired = %+v", d)
	}

	// Manual mode ignores approvals entirely.
	manual := appr.DeepCopy()
	manual.Spec.Updates.Mode = clawv1alpha1.UpdateModeManual
	if d = DesiredState(manual); d.FromApproval || d.Version != "0.4.0" {
		t.Fatalf("manual desired = %+v", d)
	}

	// Incomplete approval (missing image refs) falls back to the tag path.
	incomplete := appr.DeepCopy()
	delete(incomplete.Annotations, clawv1alpha1.AnnotationApprovedRunnerImage)
	if d = DesiredState(incomplete); d.FromApproval {
		t.Fatalf("incomplete approval must not win: %+v", d)
	}
}

func TestDegradation(t *testing.T) {
	cp := testCP(clawv1alpha1.UpdateModePrompt)
	m := Manifest{Version: "v0.5.0"}
	m.Images.Controller = "docker.io/bitwavecode/kube-claw-controller@sha256:aaa"
	m.Images.Runner = "docker.io/bitwavecode/kube-claw-runner@sha256:bbb"

	if reason, ok := Degradation(cp, m, "v0.4.0"); !ok {
		t.Fatalf("clean manifest degraded: %s", reason)
	}
	helm := m
	helm.RequiresHelmUpgrade = true
	if _, ok := Degradation(cp, helm, "v0.4.0"); ok {
		t.Fatal("requiresHelmUpgrade must degrade")
	}
	oldSup := m
	oldSup.MinSupervisorVersion = "v9.0.0"
	if _, ok := Degradation(cp, oldSup, "v0.4.0"); ok {
		t.Fatal("minSupervisorVersion must degrade")
	}
	custom := cp.DeepCopy()
	custom.Spec.Image.ControllerRepository = "ghcr.io/acme/kube-claw-controller"
	if reason, ok := Degradation(custom, m, "v0.4.0"); ok || !strings.Contains(reason, "registry") {
		t.Fatalf("custom registry must degrade, got ok=%v reason=%q", ok, reason)
	}
}

// recordingNotifier captures failure notifications.
type recordingNotifier struct{ msgs []string }

func (r *recordingNotifier) Notify(_ context.Context, _, text string) error {
	r.msgs = append(r.msgs, text)
	return nil
}

// harness builds a fake-client reconciler around a ControlPlane.
func harness(t *testing.T, cp *clawv1alpha1.ControlPlane, now *time.Time) (*Reconciler, client.Client, *recordingNotifier) {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(cp).
		WithStatusSubresource(&clawv1alpha1.ControlPlane{}).
		Build()
	n := &recordingNotifier{}
	r := &Reconciler{Client: c, Notify: n, Now: func() time.Time { return *now }}
	return r, c, n
}

func reconcile(t *testing.T, r *Reconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "claw-system", Name: "claw"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getSTS(t *testing.T, c client.Client) *appsv1.StatefulSet {
	t.Helper()
	var sts appsv1.StatefulSet
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "claw-system", Name: StatefulSetName}, &sts); err != nil {
		t.Fatalf("get sts: %v", err)
	}
	return &sts
}

func getCP(t *testing.T, c client.Client) *clawv1alpha1.ControlPlane {
	t.Helper()
	var cp clawv1alpha1.ControlPlane
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "claw-system", Name: "claw"}, &cp); err != nil {
		t.Fatalf("get cp: %v", err)
	}
	return &cp
}

// TestReconcileLifecycle drives install → approve → apply → confirm.
func TestReconcileLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	cp := testCP(clawv1alpha1.UpdateModePrompt)
	r, c, _ := harness(t, cp, &now)

	// First reconcile creates the StatefulSet at the helm-pinned tag.
	reconcile(t, r)
	sts := getSTS(t, c)
	if img := currentImage(sts); img != "docker.io/bitwavecode/kube-claw-controller:0.4.0" {
		t.Fatalf("installed image = %q", img)
	}
	if ri := currentRunnerImage(sts); ri != "docker.io/bitwavecode/kube-claw-runner:0.4.0" {
		t.Fatalf("installed runner image = %q", ri)
	}
	if got := getCP(t, c); got.Status.Phase != clawv1alpha1.PhaseIdle {
		t.Fatalf("phase after install = %q", got.Status.Phase)
	}

	// Controller boots and confirms.
	got := getCP(t, c)
	got.Status.RunningVersion = "v0.4.0"
	if err := c.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}

	// Approval lands (controller wrote annotations after the Slack click).
	got = getCP(t, c)
	got.Annotations = map[string]string{
		clawv1alpha1.AnnotationApprovedVersion:         "v0.5.0",
		clawv1alpha1.AnnotationApprovedControllerImage: "docker.io/bitwavecode/kube-claw-controller@sha256:aaa",
		clawv1alpha1.AnnotationApprovedRunnerImage:     "docker.io/bitwavecode/kube-claw-runner@sha256:bbb",
	}
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)

	sts = getSTS(t, c)
	if img := currentImage(sts); !strings.Contains(img, "@sha256:aaa") {
		t.Fatalf("updated image = %q, want digest-pinned", img)
	}
	if ri := currentRunnerImage(sts); !strings.Contains(ri, "@sha256:bbb") {
		t.Fatalf("updated runner image = %q, want digest-pinned (lockstep)", ri)
	}
	got = getCP(t, c)
	if got.Status.Phase != clawv1alpha1.PhaseUpdating || got.Status.UpdateTarget != "v0.5.0" {
		t.Fatalf("status after apply = phase %q target %q", got.Status.Phase, got.Status.UpdateTarget)
	}
	if got.Status.PreviousControllerImage != "docker.io/bitwavecode/kube-claw-controller:0.4.0" {
		t.Fatalf("previous image = %q", got.Status.PreviousControllerImage)
	}

	// New controller confirms startup → Idle.
	got.Status.RunningVersion = "v0.5.0"
	if err := c.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	got = getCP(t, c)
	if got.Status.Phase != clawv1alpha1.PhaseIdle || got.Status.UpdateTarget != "" {
		t.Fatalf("status after confirm = phase %q target %q", got.Status.Phase, got.Status.UpdateTarget)
	}
}

// TestReconcileRollback drives a failed update: deadline passes without
// confirmation → digest-pinned rollback, cleared approval, held version, and a
// failure notification; then the old version confirms → Idle.
func TestReconcileRollback(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	cp := testCP(clawv1alpha1.UpdateModePrompt)
	cp.Annotations = map[string]string{clawv1alpha1.AnnotationUpgradeAdmin: "U_ADMIN"}
	r, c, n := harness(t, cp, &now)

	reconcile(t, r) // install
	got := getCP(t, c)
	got.Status.RunningVersion = "v0.4.0"
	if err := c.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}

	// Approve v0.5.0 and apply it.
	got = getCP(t, c)
	got.Annotations[clawv1alpha1.AnnotationApprovedVersion] = "v0.5.0"
	got.Annotations[clawv1alpha1.AnnotationApprovedControllerImage] = "docker.io/bitwavecode/kube-claw-controller@sha256:bad"
	got.Annotations[clawv1alpha1.AnnotationApprovedRunnerImage] = "docker.io/bitwavecode/kube-claw-runner@sha256:bad"
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)

	// A stuck (never-Ready) controller pod from the broken rollout: Kubernetes
	// won't replace it on template revert (forced-rollback caveat) — the
	// supervisor must delete it.
	stuck := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "claw-controller-0", Namespace: "claw-system",
			Labels: map[string]string{"app.kubernetes.io/name": "claw-controller"}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}},
	}
	if err := c.Create(ctx, stuck); err != nil {
		t.Fatal(err)
	}

	// The new version never confirms; the deadline passes.
	now = now.Add(11 * time.Minute)
	reconcile(t, r)

	var gone corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Namespace: "claw-system", Name: "claw-controller-0"}, &gone); err == nil {
		t.Fatal("stuck pod must be deleted on rollback")
	}

	sts := getSTS(t, c)
	if img := currentImage(sts); img != "docker.io/bitwavecode/kube-claw-controller:0.4.0" {
		t.Fatalf("rolled-back image = %q, want the recorded previous", img)
	}
	if ri := currentRunnerImage(sts); ri != "docker.io/bitwavecode/kube-claw-runner:0.4.0" {
		t.Fatalf("rolled-back runner image = %q", ri)
	}
	got = getCP(t, c)
	if got.Status.Phase != clawv1alpha1.PhaseRollingBack {
		t.Fatalf("phase = %q, want RollingBack", got.Status.Phase)
	}
	if got.Status.LastRollback == nil || got.Status.LastRollback.From != "v0.5.0" {
		t.Fatalf("lastRollback = %+v", got.Status.LastRollback)
	}
	if _, still := got.Annotations[clawv1alpha1.AnnotationApprovedVersion]; still {
		t.Fatal("failed approval must be cleared")
	}
	if len(n.msgs) == 0 || !strings.Contains(n.msgs[0], "rolling back") {
		t.Fatalf("expected a rollback notification, got %v", n.msgs)
	}

	// Old version confirms → Idle, and the failed version stays held (no
	// re-apply even if the approval were re-added by hand — the rollback record
	// blocks it).
	got.Status.RunningVersion = "v0.4.0"
	if err := c.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	got = getCP(t, c)
	if got.Status.Phase != clawv1alpha1.PhaseIdle {
		t.Fatalf("phase after rollback confirm = %q", got.Status.Phase)
	}
	sts = getSTS(t, c)
	if img := currentImage(sts); !strings.Contains(img, ":0.4.0") {
		t.Fatalf("post-rollback image = %q", img)
	}
}

// TestReconcileMigrationHold: a migration release that misses the deadline
// holds Degraded (no rollback) and pages the admin.
func TestReconcileMigrationHold(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	cp := testCP(clawv1alpha1.UpdateModePrompt)
	cp.Annotations = map[string]string{clawv1alpha1.AnnotationUpgradeAdmin: "U_ADMIN"}
	r, c, n := harness(t, cp, &now)

	reconcile(t, r)
	got := getCP(t, c)
	got.Status.RunningVersion = "v0.4.0"
	got.Status.AvailableVersion = "v0.5.0"
	got.Status.AvailableContainsMigration = true
	if err := c.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}

	got = getCP(t, c)
	got.Annotations[clawv1alpha1.AnnotationApprovedVersion] = "v0.5.0"
	got.Annotations[clawv1alpha1.AnnotationApprovedControllerImage] = "docker.io/bitwavecode/kube-claw-controller@sha256:mig"
	got.Annotations[clawv1alpha1.AnnotationApprovedRunnerImage] = "docker.io/bitwavecode/kube-claw-runner@sha256:mig"
	if err := c.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcile(t, r)
	if got = getCP(t, c); !got.Status.UpdateContainsMigration {
		t.Fatal("update must carry the migration flag")
	}

	now = now.Add(11 * time.Minute)
	reconcile(t, r)

	got = getCP(t, c)
	if got.Status.Phase != clawv1alpha1.PhaseDegraded {
		t.Fatalf("phase = %q, want Degraded (migration hold)", got.Status.Phase)
	}
	// NOT rolled back: the (possibly migrated) new image stays.
	sts := getSTS(t, c)
	if img := currentImage(sts); !strings.Contains(img, "@sha256:mig") {
		t.Fatalf("migration hold must not roll back, image = %q", img)
	}
	if len(n.msgs) == 0 || !strings.Contains(n.msgs[0], "NOT auto-roll-back") {
		t.Fatalf("expected a degraded notification, got %v", n.msgs)
	}
}
