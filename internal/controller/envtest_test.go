package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
)

// TestAgentReconciler_Envtest exercises the reconciler against a real apiserver
// (kube-apiserver + etcd via envtest), which the fake client can't do: it
// verifies the CRD's CEL digest rule and the status subresource for real.
//
// Run with KUBEBUILDER_ASSETS set (see: setup-envtest use -p path); skipped otherwise.
func TestAgentReconciler_Envtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "charts", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	defer func() { _ = env.Stop() }()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := clawv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "claw-agents"}}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	// CEL digest rule: a tag-pinned image must be REJECTED by the apiserver.
	tagAgent := &clawv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "bad", Namespace: "claw-agents"},
		Spec:       clawv1alpha1.AgentSpec{Image: "ghcr.io/example/x:latest"},
	}
	if err := c.Create(ctx, tagAgent); err == nil {
		t.Fatal("expected CEL rejection of a tag image, but Create succeeded")
	}

	// A digest-pinned Agent is accepted; reconcile ensures supporting objects + status.
	agent := &clawv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "gcp-cost", Namespace: "claw-agents"},
		Spec: clawv1alpha1.AgentSpec{
			Image: "ghcr.io/example/gcp-cost@sha256:abc123",
			Storage: clawv1alpha1.StorageSpec{
				Workspace: &clawv1alpha1.VolumeSpec{Type: "pvc", Size: "1Gi", MountPath: "/workspace"},
			},
		},
	}
	if err := c.Create(ctx, agent); err != nil {
		t.Fatalf("create digest agent: %v", err)
	}

	r := &AgentReconciler{Client: c, Scheme: scheme}
	key := types.NamespacedName{Name: "gcp-cost", Namespace: "claw-agents"}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Supporting objects exist.
	mustExist(t, ctx, c, &corev1.ServiceAccount{}, "claw-agent-gcp-cost")
	mustExist(t, ctx, c, &corev1.PersistentVolumeClaim{}, "gcp-cost-workspace")
	mustExist(t, ctx, c, &networkingv1.NetworkPolicy{}, "gcp-cost-agent")

	// Status (subresource) populated.
	var got clawv1alpha1.Agent
	if err := c.Get(ctx, key, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.SelectedImageDigest != "sha256:abc123" || got.Status.AgentSpecHash == "" {
		t.Fatalf("status not populated: %+v", got.Status)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, "Ready"); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %v", cond)
	}
}

func mustExist(t *testing.T, ctx context.Context, c client.Client, obj client.Object, name string) {
	t.Helper()
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "claw-agents"}, obj); err != nil {
		t.Fatalf("expected %s to exist: %v", name, err)
	}
}
