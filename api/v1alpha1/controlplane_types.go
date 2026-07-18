package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ControlPlane is the self-update plane's coordination resource (DESIGN.md §24).
// It is operator infrastructure, NOT a user-facing resource (Agent remains the
// only one of those): the chart renders exactly one, named "claw", in the
// release namespace, and humans normally touch it only through Helm values.
//
// Ownership split (§24.2, load-bearing):
//   - spec   = Helm-owned POLICY. A `helm upgrade` may rewrite it wholesale.
//   - annotations (Annotation*) = APPROVALS, written by the controller on a
//     Slack approval. Helm never templates them, so its three-way merge
//     preserves them across upgrades.
//   - status = STATE, written by the supervisor (detection, update progress,
//     rollbacks) and the controller (runningVersion = startup-confirmed).

// Update modes (spec.updates.mode): who may move the desired version.
const (
	// UpdateModePrompt — new releases are offered to the upgrade admin in
	// Slack; the supervisor applies on approval. The default.
	UpdateModePrompt = "prompt"
	// UpdateModeAuto — the supervisor applies new releases unprompted
	// (digest-pinned, health-watched); the controller announces afterwards.
	UpdateModeAuto = "auto"
	// UpdateModeManual — only spec.version (i.e. Helm) moves the install.
	// New releases are still announced, never self-applied.
	UpdateModeManual = "manual"
)

// ControlPlane phases (status.phase).
const (
	PhaseIdle        = "Idle"
	PhaseUpdating    = "Updating"
	PhaseRollingBack = "RollingBack"
	PhaseDegraded    = "Degraded"
)

// Approval + coordination annotations on the ControlPlane object. Written by
// the CONTROLLER (approvals, admin mirror); read by the supervisor. Never
// rendered by Helm.
const (
	// AnnotationApprovedVersion is the release version the upgrade admin
	// approved (e.g. "v0.4.0").
	AnnotationApprovedVersion = "claw.run/approved-version"
	// AnnotationApprovedControllerImage is the digest-pinned controller image
	// from the release manifest, captured at approval time (no tag TOCTOU).
	AnnotationApprovedControllerImage = "claw.run/approved-controller-image"
	// AnnotationApprovedRunnerImage is the digest-pinned runner image, ditto.
	AnnotationApprovedRunnerImage = "claw.run/approved-runner-image"
	// AnnotationApprovedBy records who approved (Slack user id / "cli").
	AnnotationApprovedBy = "claw.run/approved-by"
	// AnnotationUpgradeAdmin mirrors the store's upgrade-admin setting so the
	// supervisor (which has no store access) can DM rollback failures.
	AnnotationUpgradeAdmin = "claw.run/upgrade-admin"
	// AnnotationMgmtChannel mirrors the store's management-channel setting so
	// the supervisor can post failures there too.
	AnnotationMgmtChannel = "claw.run/management-channel"
)

// ControlPlaneSpec is Helm-owned policy: which release stream to follow, who
// may move the version, and the controller deployment knobs (transcribed from
// values.yaml — the supervisor renders the controller StatefulSet from these).
type ControlPlaneSpec struct {
	// Updates configures the self-update plane.
	// +optional
	Updates UpdatesSpec `json:"updates,omitempty"`

	// Version is the Helm-pinned release floor — the image tag deployed at
	// install and the version `helm upgrade` moves. In prompt/auto modes the
	// effective version is semver-max(version, approved annotation); in manual
	// mode this field alone decides (DESIGN.md §24.2).
	Version string `json:"version"`

	// Image is where release images live. Digest-pinned refs from the release
	// manifest override tag derivation for self-applied updates.
	// +optional
	Image ImagesSpec `json:"image,omitempty"`

	// Controller carries the controller StatefulSet knobs (values passthrough).
	// +optional
	Controller ControllerConfig `json:"controller,omitempty"`

	// Slack mirrors the chart's slack.* values (token secret + static routes).
	// +optional
	Slack SlackConfig `json:"slack,omitempty"`

	// Service mirrors the chart's service ports (the Service object itself
	// stays Helm-rendered; the StatefulSet's containerPorts must match it).
	// +optional
	Service PortsConfig `json:"service,omitempty"`
}

// UpdatesSpec is the self-update policy.
type UpdatesSpec struct {
	// Mode is who may move the desired version (DESIGN.md §24.4).
	// +kubebuilder:validation:Enum=prompt;auto;manual
	// +kubebuilder:default=prompt
	// +optional
	Mode string `json:"mode,omitempty"`

	// Channel is the release stream to follow.
	// +kubebuilder:default=stable
	// +optional
	Channel string `json:"channel,omitempty"`

	// ManifestURL overrides the per-channel release manifest URL (required for
	// custom registries — DESIGN.md §24.3).
	// +optional
	ManifestURL string `json:"manifestURL,omitempty"`

	// CheckInterval is how often the supervisor polls the release manifest.
	// +kubebuilder:default="6h"
	// +optional
	CheckInterval string `json:"checkInterval,omitempty"`

	// ConfirmDeadline is how long the watchdog waits for the new controller to
	// confirm startup (status.runningVersion) before rolling back (§24.5).
	// +kubebuilder:default="10m"
	// +optional
	ConfirmDeadline string `json:"confirmDeadline,omitempty"`
}

// ImagesSpec is the image source configuration.
type ImagesSpec struct {
	// ControllerRepository is the controller image repository (no tag).
	// +kubebuilder:default="docker.io/bitwavecode/kube-claw-controller"
	// +optional
	ControllerRepository string `json:"controllerRepository,omitempty"`

	// RunnerRepository is the runner image repository (no tag).
	// +kubebuilder:default="docker.io/bitwavecode/kube-claw-runner"
	// +optional
	RunnerRepository string `json:"runnerRepository,omitempty"`

	// PullPolicy for the controller pod.
	// +kubebuilder:default=IfNotPresent
	// +optional
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

// ControllerConfig carries the controller StatefulSet knobs (values passthrough,
// mirrors charts/claw/values.yaml controller.*).
type ControllerConfig struct {
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// +kubebuilder:default="/var/lib/claw"
	// +optional
	DataDir string `json:"dataDir,omitempty"`

	// +optional
	EnableRouter *bool `json:"enableRouter,omitempty"`

	// +kubebuilder:validation:Enum=console;json
	// +kubebuilder:default=console
	// +optional
	LogFormat string `json:"logFormat,omitempty"`

	// SelfURL is the in-cluster URL run pods use to reach the controller.
	// +optional
	SelfURL string `json:"selfURL,omitempty"`

	// UIBaseURL is the public base URL of the secret-intake UI.
	// +optional
	UIBaseURL string `json:"uiBaseURL,omitempty"`

	// AdminSecretName holds the admin dashboard basic-auth password.
	// +kubebuilder:default="claw-admin"
	// +optional
	AdminSecretName string `json:"adminSecretName,omitempty"`

	// RestrictAgentEgress opts into the deny-east/west agent NetworkPolicy.
	// +optional
	RestrictAgentEgress bool `json:"restrictAgentEgress,omitempty"`

	// Artifacts configures shareable-document link lifetimes.
	// +optional
	Artifacts *ArtifactsConfig `json:"artifacts,omitempty"`

	// Resources for the controller container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Storage sizes the controller's data PVC (volumeClaimTemplate — immutable
	// after creation, like any StatefulSet volumeClaimTemplate).
	// +optional
	Storage ControlPlaneStorage `json:"storage,omitempty"`
}

// ArtifactsConfig mirrors the chart's controller.artifacts values.
type ArtifactsConfig struct {
	// TTL is the default share-link lifetime (Go duration, e.g. "24h").
	// +kubebuilder:default="24h"
	// +optional
	TTL string `json:"ttl,omitempty"`
	// MaxTTL caps per-publish lifetime overrides.
	// +kubebuilder:default="168h"
	// +optional
	MaxTTL string `json:"maxTTL,omitempty"`
}

// ControlPlaneStorage sizes the controller data volume.
type ControlPlaneStorage struct {
	// +kubebuilder:default="20Gi"
	// +optional
	Size string `json:"size,omitempty"`
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
}

// SlackConfig mirrors the chart's slack.* values.
type SlackConfig struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// +kubebuilder:default="claw-slack-tokens"
	// +optional
	TokenSecretName string `json:"tokenSecretName,omitempty"`
	// RoutesJSON is the optional static channel→agent routing (CLAW_SLACK_ROUTES).
	// +optional
	RoutesJSON string `json:"routesJSON,omitempty"`
}

// PortsConfig mirrors the chart's service.* ports.
type PortsConfig struct {
	// +kubebuilder:default=8443
	// +optional
	APIPort int32 `json:"apiPort,omitempty"`
	// +kubebuilder:default=8080
	// +optional
	MetricsPort int32 `json:"metricsPort,omitempty"`
	// +kubebuilder:default=8081
	// +optional
	ProbePort int32 `json:"probePort,omitempty"`
	// +kubebuilder:default=8090
	// +optional
	UIPort int32 `json:"uiPort,omitempty"`
}

// RollbackRecord describes the last automatic rollback (status.lastRollback).
type RollbackRecord struct {
	From   string      `json:"from,omitempty"`
	To     string      `json:"to,omitempty"`
	Reason string      `json:"reason,omitempty"`
	At     metav1.Time `json:"at,omitempty"`
}

// ControlPlaneStatus is runtime state. The supervisor owns everything except
// RunningVersion, which the CONTROLLER writes after boot-complete (migrations
// ran, store open, Slack connected when enabled) — that write IS the
// startup-confirmed signal the watchdog waits on (DESIGN.md §24.5).
type ControlPlaneStatus struct {
	// RunningVersion is the release version the controller confirmed at boot.
	// +optional
	RunningVersion string `json:"runningVersion,omitempty"`

	// RunningControllerImage / RunningRunnerImage are the image refs the
	// supervisor most recently applied (tag- or digest-pinned).
	// +optional
	RunningControllerImage string `json:"runningControllerImage,omitempty"`
	// +optional
	RunningRunnerImage string `json:"runningRunnerImage,omitempty"`

	// AvailableVersion is the newest release the manifest offers (poller).
	// +optional
	AvailableVersion string `json:"availableVersion,omitempty"`
	// AvailableNotes is the release's human summary (used in the Slack prompt).
	// +optional
	AvailableNotes string `json:"availableNotes,omitempty"`
	// AvailableContainsMigration mirrors the manifest's migration flag.
	// +optional
	AvailableContainsMigration bool `json:"availableContainsMigration,omitempty"`
	// AvailableControllerImage / AvailableRunnerImage are the digest-pinned
	// refs from the manifest — an approval copies them into the annotations.
	// +optional
	AvailableControllerImage string `json:"availableControllerImage,omitempty"`
	// +optional
	AvailableRunnerImage string `json:"availableRunnerImage,omitempty"`
	// AvailableRequiresHelm is true when the release can't be self-applied
	// (requiresHelmUpgrade / minSupervisorVersion / custom registry) — the
	// prompt degrades to notify-only in every mode (DESIGN.md §24.3).
	// +optional
	AvailableRequiresHelm bool `json:"availableRequiresHelm,omitempty"`
	// AvailableRequiresHelmReason says why (for the Slack message).
	// +optional
	AvailableRequiresHelmReason string `json:"availableRequiresHelmReason,omitempty"`

	// LastCheckTime is when the manifest was last polled successfully.
	// +optional
	LastCheckTime *metav1.Time `json:"lastCheckTime,omitempty"`

	// Phase is the update lifecycle phase (Idle/Updating/RollingBack/Degraded).
	// +optional
	Phase string `json:"phase,omitempty"`

	// UpdateTarget is the version currently being applied (phase=Updating).
	// +optional
	UpdateTarget string `json:"updateTarget,omitempty"`
	// UpdateStartedAt starts the watchdog's confirm deadline.
	// +optional
	UpdateStartedAt *metav1.Time `json:"updateStartedAt,omitempty"`
	// UpdateContainsMigration disables auto-rollback for this update (§24.5).
	// +optional
	UpdateContainsMigration bool `json:"updateContainsMigration,omitempty"`

	// PreviousControllerImage / PreviousRunnerImage are the rollback target,
	// recorded (not re-resolved) before each apply.
	// +optional
	PreviousControllerImage string `json:"previousControllerImage,omitempty"`
	// +optional
	PreviousRunnerImage string `json:"previousRunnerImage,omitempty"`
	// PreviousVersion is the version the Previous* images ran.
	// +optional
	PreviousVersion string `json:"previousVersion,omitempty"`

	// LastRollback describes the most recent automatic rollback.
	// +optional
	LastRollback *RollbackRecord `json:"lastRollback,omitempty"`

	// Conditions follow the standard Kubernetes condition convention.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cp
// +kubebuilder:printcolumn:name="Running",type=string,JSONPath=`.status.runningVersion`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.availableVersion`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.updates.mode`

// ControlPlane is the self-update plane's coordination resource (DESIGN.md §24).
type ControlPlane struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ControlPlaneSpec   `json:"spec,omitempty"`
	Status ControlPlaneStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ControlPlaneList contains a list of ControlPlane.
type ControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ControlPlane `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ControlPlane{}, &ControlPlaneList{})
}
