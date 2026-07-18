package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
)

// TestSupervisor_Envtest exercises the ControlPlane reconciler against a real
// apiserver (Phase 8b AC): the CRD's defaulting applies for real, the created
// StatefulSet is valid (volumeClaimTemplates, probes, selectors), and an
// approval-annotation mutation rolls the controller image digest-pinned.
//
// Run with KUBEBUILDER_ASSETS set (see: setup-envtest use -p path); skipped otherwise.
func TestSupervisor_Envtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "charts", "claw", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	defer func() { _ = env.Stop() }()

	c, err := client.New(cfg, client.Options{Scheme: testScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "claw-system"}}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	// Minimal spec: the apiserver fills the CRD defaults (mode=prompt, ports,
	// storage size, …) — this validates the kubebuilder markers for real.
	cp := &clawv1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "claw", Namespace: "claw-system"},
		Spec:       clawv1alpha1.ControlPlaneSpec{Version: "0.4.0"},
	}
	if err := c.Create(ctx, cp); err != nil {
		t.Fatalf("create controlplane: %v", err)
	}
	var defaulted clawv1alpha1.ControlPlane
	key := types.NamespacedName{Namespace: "claw-system", Name: "claw"}
	if err := c.Get(ctx, key, &defaulted); err != nil {
		t.Fatal(err)
	}
	if defaulted.Spec.Updates.Mode != clawv1alpha1.UpdateModePrompt || defaulted.Spec.Service.APIPort != 8443 {
		t.Fatalf("CRD defaults not applied: %+v", defaulted.Spec)
	}

	r := &Reconciler{Client: c}
	req := ctrl.Request{NamespacedName: key}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (install): %v", err)
	}

	var sts appsv1.StatefulSet
	stsKey := types.NamespacedName{Namespace: "claw-system", Name: StatefulSetName}
	if err := c.Get(ctx, stsKey, &sts); err != nil {
		t.Fatalf("statefulset not created: %v", err)
	}
	if img := currentImage(&sts); img != "docker.io/bitwavecode/kube-claw-controller:0.4.0" {
		t.Fatalf("installed image = %q", img)
	}
	if sts.Spec.Template.Labels["app.kubernetes.io/name"] != "claw-controller" {
		t.Fatal("pod labels must keep the Service/NetworkPolicy selector")
	}

	// CR mutation (a digest-pinned approval) rolls the StatefulSet.
	if err := c.Get(ctx, key, &defaulted); err != nil {
		t.Fatal(err)
	}
	defaulted.Annotations = map[string]string{
		clawv1alpha1.AnnotationApprovedVersion:         "v0.5.0",
		clawv1alpha1.AnnotationApprovedControllerImage: "docker.io/bitwavecode/kube-claw-controller@sha256:aaa",
		clawv1alpha1.AnnotationApprovedRunnerImage:     "docker.io/bitwavecode/kube-claw-runner@sha256:bbb",
	}
	if err := c.Update(ctx, &defaulted); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (update): %v", err)
	}
	if err := c.Get(ctx, stsKey, &sts); err != nil {
		t.Fatal(err)
	}
	if img := currentImage(&sts); !strings.Contains(img, "@sha256:aaa") {
		t.Fatalf("updated image = %q, want digest-pinned", img)
	}
	if ri := currentRunnerImage(&sts); !strings.Contains(ri, "@sha256:bbb") {
		t.Fatalf("runner image = %q, want lockstep digest", ri)
	}
	var after clawv1alpha1.ControlPlane
	if err := c.Get(ctx, key, &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.Phase != clawv1alpha1.PhaseUpdating || after.Status.UpdateTarget != "v0.5.0" {
		t.Fatalf("status = phase %q target %q", after.Status.Phase, after.Status.UpdateTarget)
	}

	// The controller confirms startup (status subresource write) → Idle.
	after.Status.RunningVersion = "v0.5.0"
	if err := c.Status().Update(ctx, &after); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile (confirm): %v", err)
	}
	if err := c.Get(ctx, key, &after); err != nil {
		t.Fatal(err)
	}
	if after.Status.Phase != clawv1alpha1.PhaseIdle {
		t.Fatalf("phase after confirm = %q", after.Status.Phase)
	}
}
