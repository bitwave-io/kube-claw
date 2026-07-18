package upgrade

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

// fakePoster records upgrade prompts/announcements.
type fakePoster struct {
	prompts   []string // "<available>|canApply=<bool>|reason=<reason>"
	announces []string
}

func (f *fakePoster) PostUpgradePrompt(_ context.Context, _, _, available, _, reason string, _, canApply bool) error {
	f.prompts = append(f.prompts, available+"|canApply="+boolStr(canApply)+"|reason="+reason)
	return nil
}
func (f *fakePoster) PostReply(_ context.Context, _, _, text string) error {
	f.announces = append(f.announces, text)
	return nil
}
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func testCoordinator(t *testing.T, cp *clawv1alpha1.ControlPlane) (*Coordinator, client.Client, *fakePoster) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := clawv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cp).
		WithStatusSubresource(&clawv1alpha1.ControlPlane{}).Build()

	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	poster := &fakePoster{}
	coord := &Coordinator{
		Store: st, Reader: c, Writer: c, Notifier: poster,
		Name: "claw", Namespace: "claw-system", RunningVersion: "v0.4.0",
	}
	return coord, c, poster
}

func setAdmin(t *testing.T, coord *Coordinator, admin string) {
	t.Helper()
	if err := coord.Store.Tx(context.Background(), func(tx store.Tx) error {
		return tx.SetSetting(store.SettingUpgradeAdmin, admin)
	}); err != nil {
		t.Fatal(err)
	}
}

func baseCP() *clawv1alpha1.ControlPlane {
	return &clawv1alpha1.ControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "claw", Namespace: "claw-system"},
		Spec: clawv1alpha1.ControlPlaneSpec{
			Version: "0.4.0",
			Updates: clawv1alpha1.UpdatesSpec{Mode: clawv1alpha1.UpdateModePrompt, CheckInterval: "6h"},
		},
	}
}

func getCP(t *testing.T, c client.Client) *clawv1alpha1.ControlPlane {
	t.Helper()
	var cp clawv1alpha1.ControlPlane
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "claw-system", Name: "claw"}, &cp); err != nil {
		t.Fatal(err)
	}
	return &cp
}

// TestConfirmStartup: writes runningVersion (the startup-confirmed signal) and
// announces version changes to the admin.
func TestConfirmStartup(t *testing.T) {
	ctx := context.Background()
	cp := baseCP()
	cp.Status.RunningVersion = "v0.3.1" // the old version's write
	coord, c, poster := testCoordinator(t, cp)
	setAdmin(t, coord, "U_ADMIN")

	if err := coord.ConfirmStartup(ctx); err != nil {
		t.Fatal(err)
	}
	if got := getCP(t, c); got.Status.RunningVersion != "v0.4.0" {
		t.Fatalf("runningVersion = %q", got.Status.RunningVersion)
	}
	if len(poster.announces) != 1 || !strings.Contains(poster.announces[0], "v0.4.0") || !strings.Contains(poster.announces[0], "v0.3.1") {
		t.Fatalf("announce = %v", poster.announces)
	}

	// Same version again: no duplicate announce.
	if err := coord.ConfirmStartup(ctx); err != nil {
		t.Fatal(err)
	}
	if len(poster.announces) != 1 {
		t.Fatalf("duplicate announce: %v", poster.announces)
	}
}

// TestPromptFlow: a newly available release prompts the admin exactly once;
// skip suppresses it permanently, Later temporarily.
func TestPromptFlow(t *testing.T) {
	ctx := context.Background()
	cp := baseCP()
	cp.Status.RunningVersion = "v0.4.0"
	cp.Status.AvailableVersion = "v0.5.0"
	cp.Status.AvailableControllerImage = "repo/controller@sha256:aaa"
	cp.Status.AvailableRunnerImage = "repo/runner@sha256:bbb"
	coord, c, poster := testCoordinator(t, cp)
	setAdmin(t, coord, "U_ADMIN")

	if err := coord.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(poster.prompts) != 1 || !strings.HasPrefix(poster.prompts[0], "v0.5.0|canApply=true") {
		t.Fatalf("prompts = %v", poster.prompts)
	}
	// Admin annotation mirrored for the supervisor's failure notifications.
	if got := getCP(t, c); got.Annotations[clawv1alpha1.AnnotationUpgradeAdmin] != "U_ADMIN" {
		t.Fatalf("admin mirror = %q", got.Annotations[clawv1alpha1.AnnotationUpgradeAdmin])
	}

	// Second tick: notified marker suppresses a re-prompt.
	if err := coord.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(poster.prompts) != 1 {
		t.Fatalf("re-prompted: %v", poster.prompts)
	}

	// Later: remind window suppresses even after the notified marker clears.
	if err := coord.Later(ctx, "v0.5.0"); err != nil {
		t.Fatal(err)
	}
	if err := coord.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(poster.prompts) != 1 {
		t.Fatalf("prompted during remind window: %v", poster.prompts)
	}

	// Skip: never again for this version.
	if err := coord.Skip(ctx, "v0.5.0", "U_ADMIN"); err != nil {
		t.Fatal(err)
	}
	// Force the remind window open and the notified marker clear.
	if err := coord.setSetting(ctx, store.SettingRemindAfter, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := coord.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(poster.prompts) != 1 {
		t.Fatalf("prompted a skipped version: %v", poster.prompts)
	}
}

// TestPromptDegradations: manual mode and requiresHelm releases get
// notify-only prompts (no self-apply button).
func TestPromptDegradations(t *testing.T) {
	ctx := context.Background()

	manual := baseCP()
	manual.Spec.Updates.Mode = clawv1alpha1.UpdateModeManual
	manual.Status.RunningVersion = "v0.4.0"
	manual.Status.AvailableVersion = "v0.5.0"
	coord, _, poster := testCoordinator(t, manual)
	setAdmin(t, coord, "U_ADMIN")
	if err := coord.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(poster.prompts) != 1 || !strings.Contains(poster.prompts[0], "canApply=false") ||
		!strings.Contains(poster.prompts[0], "helm-managed") {
		t.Fatalf("manual-mode prompt = %v", poster.prompts)
	}

	helmOnly := baseCP()
	helmOnly.Status.RunningVersion = "v0.4.0"
	helmOnly.Status.AvailableVersion = "v0.5.0"
	helmOnly.Status.AvailableRequiresHelm = true
	helmOnly.Status.AvailableRequiresHelmReason = "this release changes the chart — it requires a helm upgrade"
	coord2, _, poster2 := testCoordinator(t, helmOnly)
	setAdmin(t, coord2, "U_ADMIN")
	if err := coord2.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(poster2.prompts) != 1 || !strings.Contains(poster2.prompts[0], "canApply=false") {
		t.Fatalf("requires-helm prompt = %v", poster2.prompts)
	}
}

// TestApprove: writes the digest-pinned annotations; refuses versions that
// aren't the offered release or can't self-apply.
func TestApprove(t *testing.T) {
	ctx := context.Background()
	cp := baseCP()
	cp.Status.AvailableVersion = "v0.5.0"
	cp.Status.AvailableControllerImage = "repo/controller@sha256:aaa"
	cp.Status.AvailableRunnerImage = "repo/runner@sha256:bbb"
	coord, c, _ := testCoordinator(t, cp)

	if err := coord.Approve(ctx, "v0.9.9", "U_ADMIN"); !errors.Is(err, ErrNotOffered) {
		t.Fatalf("stale approve err = %v, want ErrNotOffered", err)
	}
	if err := coord.Approve(ctx, "v0.5.0", "U_ADMIN"); err != nil {
		t.Fatal(err)
	}
	got := getCP(t, c)
	if got.Annotations[clawv1alpha1.AnnotationApprovedVersion] != "v0.5.0" ||
		got.Annotations[clawv1alpha1.AnnotationApprovedControllerImage] != "repo/controller@sha256:aaa" ||
		got.Annotations[clawv1alpha1.AnnotationApprovedRunnerImage] != "repo/runner@sha256:bbb" ||
		got.Annotations[clawv1alpha1.AnnotationApprovedBy] != "U_ADMIN" {
		t.Fatalf("approval annotations = %v", got.Annotations)
	}

	// A requires-helm release refuses approval outright.
	helmOnly := baseCP()
	helmOnly.Status.AvailableVersion = "v0.5.0"
	helmOnly.Status.AvailableRequiresHelm = true
	helmOnly.Status.AvailableRequiresHelmReason = "chart changed"
	coord2, _, _ := testCoordinator(t, helmOnly)
	if err := coord2.Approve(ctx, "v0.5.0", "U_ADMIN"); err == nil || !strings.Contains(err.Error(), "chart changed") {
		t.Fatalf("requires-helm approve err = %v", err)
	}
}
