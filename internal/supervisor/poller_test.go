package supervisor

import (
	"context"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
)

func testManifest(v string) Manifest {
	m := Manifest{SchemaVersion: 1, Channel: "stable", Version: v}
	m.Images.Controller = "docker.io/bitwavecode/kube-claw-controller@sha256:aaa"
	m.Images.Runner = "docker.io/bitwavecode/kube-claw-runner@sha256:bbb"
	m.Notes = "test release"
	return m
}

// TestPollerRecordsAvailable: detection writes Available* status in every mode
// and doesn't approve anything outside auto mode.
func TestPollerRecordsAvailable(t *testing.T) {
	ctx := context.Background()
	cp := testCP(clawv1alpha1.UpdateModePrompt)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cp).
		WithStatusSubresource(&clawv1alpha1.ControlPlane{}).Build()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	p := &Poller{Client: c, Namespace: "claw-system",
		Fetch: func(context.Context, string) (Manifest, error) { return testManifest("v0.5.0"), nil },
		Now:   func() time.Time { return now }}

	p.pollAll(ctx)

	got := getCP(t, c)
	if got.Status.AvailableVersion != "v0.5.0" || got.Status.AvailableNotes != "test release" {
		t.Fatalf("available = %q notes %q", got.Status.AvailableVersion, got.Status.AvailableNotes)
	}
	if got.Status.AvailableRequiresHelm {
		t.Fatalf("clean manifest flagged requires-helm: %s", got.Status.AvailableRequiresHelmReason)
	}
	if got.Status.LastCheckTime == nil {
		t.Fatal("lastCheckTime not recorded")
	}
	if _, approved := got.Annotations[clawv1alpha1.AnnotationApprovedVersion]; approved {
		t.Fatal("prompt mode must not self-approve")
	}

	// Within the interval → not due, no refetch (Fetch panics if called).
	p.Fetch = func(context.Context, string) (Manifest, error) { panic("must not refetch") }
	p.pollAll(ctx)
}

// TestPollerAutoApproves: auto mode writes the digest-pinned approval for a
// newer, applicable release — and holds off on rolled-back versions.
func TestPollerAutoApproves(t *testing.T) {
	ctx := context.Background()
	cp := testCP(clawv1alpha1.UpdateModeAuto)
	cp.Status.RunningVersion = "v0.4.0"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cp).
		WithStatusSubresource(&clawv1alpha1.ControlPlane{}).Build()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	p := &Poller{Client: c, Namespace: "claw-system",
		Fetch: func(context.Context, string) (Manifest, error) { return testManifest("v0.5.0"), nil },
		Now:   func() time.Time { return now }}

	p.pollAll(ctx)
	got := getCP(t, c)
	if got.Annotations[clawv1alpha1.AnnotationApprovedVersion] != "v0.5.0" {
		t.Fatalf("auto mode approval = %q", got.Annotations[clawv1alpha1.AnnotationApprovedVersion])
	}
	if got.Annotations[clawv1alpha1.AnnotationApprovedBy] != "auto" {
		t.Fatalf("approvedBy = %q", got.Annotations[clawv1alpha1.AnnotationApprovedBy])
	}

	// A version that rolled back here is never auto-re-approved.
	cp2 := testCP(clawv1alpha1.UpdateModeAuto)
	cp2.Status.LastRollback = &clawv1alpha1.RollbackRecord{From: "v0.5.0"}
	c2 := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cp2).
		WithStatusSubresource(&clawv1alpha1.ControlPlane{}).Build()
	p2 := &Poller{Client: c2, Namespace: "claw-system",
		Fetch: func(context.Context, string) (Manifest, error) { return testManifest("v0.5.0"), nil },
		Now:   func() time.Time { return now }}
	p2.pollAll(ctx)
	if got := getCP(t, c2); got.Annotations[clawv1alpha1.AnnotationApprovedVersion] != "" {
		t.Fatal("rolled-back version must not be auto-re-approved")
	}

	// A requires-helm release is recorded but never auto-approved.
	cp3 := testCP(clawv1alpha1.UpdateModeAuto)
	c3 := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cp3).
		WithStatusSubresource(&clawv1alpha1.ControlPlane{}).Build()
	helmOnly := testManifest("v0.5.0")
	helmOnly.RequiresHelmUpgrade = true
	p3 := &Poller{Client: c3, Namespace: "claw-system",
		Fetch: func(context.Context, string) (Manifest, error) { return helmOnly, nil },
		Now:   func() time.Time { return now }}
	p3.pollAll(ctx)
	got3 := getCP(t, c3)
	if !got3.Status.AvailableRequiresHelm {
		t.Fatal("requires-helm not recorded")
	}
	if got3.Annotations[clawv1alpha1.AnnotationApprovedVersion] != "" {
		t.Fatal("requires-helm release must not be auto-approved")
	}
}
