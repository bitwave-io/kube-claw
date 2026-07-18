// Package supervisor is the self-update plane (DESIGN.md §24): a tiny,
// always-running reconciler that owns the controller StatefulSet, applies
// approved releases, health-watches rollouts, and rolls back failures. It is
// deliberately boring — no Slack socket, no LLM, no store, no secret-authority
// powers; its only Slack capability is one bare chat.postMessage on failure.
package supervisor

import (
	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/version"
)

// Desired is the version + image refs the controller StatefulSet should run.
type Desired struct {
	Version         string
	ControllerImage string
	RunnerImage     string
	// FromApproval is true when a digest-pinned Slack/CLI approval decided the
	// version (vs. the Helm-pinned spec.version tag path).
	FromApproval bool
}

// DesiredState resolves the desired version per DESIGN.md §24.2:
//
//	manual mode:      desired = spec.version
//	prompt/auto mode: desired = semver-max(spec.version, approved annotation)
//
// Helm-pinned versions deploy by TAG (repository:version); approvals deploy the
// digest-pinned image refs captured at approval time (deliberate asymmetry —
// what the admin approved is byte-for-byte what runs).
func DesiredState(cp *clawv1alpha1.ControlPlane) Desired {
	spec := cp.Spec
	d := Desired{
		Version:         spec.Version,
		ControllerImage: controllerRepo(spec) + ":" + spec.Version,
		RunnerImage:     runnerRepo(spec) + ":" + spec.Version,
	}
	if spec.Updates.Mode == clawv1alpha1.UpdateModeManual {
		return d
	}
	approved := cp.Annotations[clawv1alpha1.AnnotationApprovedVersion]
	ctrlImg := cp.Annotations[clawv1alpha1.AnnotationApprovedControllerImage]
	runImg := cp.Annotations[clawv1alpha1.AnnotationApprovedRunnerImage]
	// The approval wins only when strictly newer than the Helm floor (a later
	// helm upgrade moves past a stale approval) and complete (both images).
	if approved == "" || ctrlImg == "" || runImg == "" || !version.Newer(approved, spec.Version) {
		return d
	}
	return Desired{Version: approved, ControllerImage: ctrlImg, RunnerImage: runImg, FromApproval: true}
}

func controllerRepo(spec clawv1alpha1.ControlPlaneSpec) string {
	if spec.Image.ControllerRepository != "" {
		return spec.Image.ControllerRepository
	}
	return "docker.io/bitwavecode/kube-claw-controller"
}

func runnerRepo(spec clawv1alpha1.ControlPlaneSpec) string {
	if spec.Image.RunnerRepository != "" {
		return spec.Image.RunnerRepository
	}
	return "docker.io/bitwavecode/kube-claw-runner"
}
