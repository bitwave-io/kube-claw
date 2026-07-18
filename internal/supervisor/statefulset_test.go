package supervisor

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
)

func buildArgs(t *testing.T, cp *clawv1alpha1.ControlPlane) []string {
	t.Helper()
	sts := BuildStatefulSet(cp, Desired{ControllerImage: "ctrl:1", RunnerImage: "run:1"})
	return sts.Spec.Template.Spec.Containers[0].Args
}

func TestBuildStatefulSetArtifactArgs(t *testing.T) {
	cp := &clawv1alpha1.ControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "claw", Namespace: "claw-system"}}

	// Without an artifacts block the flags are omitted (controller defaults apply).
	for _, a := range buildArgs(t, cp) {
		if a == "--artifact-ttl=24h" || a == "--artifact-max-ttl=168h" {
			t.Fatalf("artifact flag %q rendered without an artifacts block", a)
		}
	}

	cp.Spec.Controller.Artifacts = &clawv1alpha1.ArtifactsConfig{TTL: "12h", MaxTTL: "72h"}
	args := buildArgs(t, cp)
	for _, want := range []string{"--artifact-ttl=12h", "--artifact-max-ttl=72h"} {
		if !slices.Contains(args, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}

	// Empty fields fall back to the chart defaults.
	cp.Spec.Controller.Artifacts = &clawv1alpha1.ArtifactsConfig{}
	args = buildArgs(t, cp)
	for _, want := range []string{"--artifact-ttl=24h", "--artifact-max-ttl=168h"} {
		if !slices.Contains(args, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
}
