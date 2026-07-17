package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentSpec is the desired state of an Agent.
//
// The Agent is the only kube-claw CRD: base image, storage, and secret refs are
// inline fields here rather than separate CRDs (DESIGN.md §6).
type AgentSpec struct {
	// DisplayName is a human-friendly label shown in status and Slack.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Image is the agent runtime image. When set it MUST be pinned to a digest
	// (contains "@sha256:") so secret grants bind to immutable code — a tag is
	// rejected at apply time. Optional when BaseImageRef is used instead
	// (DESIGN.md §6, §9, §23).
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == '' || self.contains('@sha256:')",message="image must be empty or pinned to a digest (@sha256:)"
	Image string `json:"image,omitempty"`

	// BaseImageRef names a registered base image (BaseImage registry) the
	// controller resolves to a concrete image. Takes precedence over Image.
	// +optional
	BaseImageRef string `json:"baseImageRef,omitempty"`

	// Runtime controls scale-to-zero session behavior.
	// +optional
	Runtime RuntimeSpec `json:"runtime,omitempty"`

	// Storage declares workspace/memory/cache volumes (inline; no StorageProfile CRD).
	// +optional
	Storage StorageSpec `json:"storage,omitempty"`

	// Secrets are the secret references and delivery specs this agent may request.
	// Values never live here — only names + delivery (DESIGN.md §6, §8).
	// +optional
	Secrets []SecretRef `json:"secrets,omitempty"`

	// Model configures the LLM provider and system prompt.
	// +optional
	Model *ModelSpec `json:"model,omitempty"`

	// Network restricts agent pod egress.
	// +optional
	Network NetworkSpec `json:"network,omitempty"`

	// Command overrides the pod entrypoint. Defaults to the bootstrap+runner
	// contract (DESIGN.md §11).
	// +optional
	Command []string `json:"command,omitempty"`
}

// RuntimeSpec controls the agent runtime mode and idle behavior.
type RuntimeSpec struct {
	// Mode is the runtime mode. Only scaleToZeroSession is supported in v0.
	// +kubebuilder:validation:Enum=scaleToZeroSession
	// +kubebuilder:default=scaleToZeroSession
	// +optional
	Mode string `json:"mode,omitempty"`

	// IdleTimeout is how long a session pod stays warm before scaling to zero.
	// +kubebuilder:default="15m"
	// +optional
	IdleTimeout string `json:"idleTimeout,omitempty"`

	// ColdStartReply is the message posted to Slack while a sleeping agent wakes.
	// +optional
	ColdStartReply string `json:"coldStartReply,omitempty"`
}

// StorageSpec declares the agent's persistent and ephemeral volumes.
type StorageSpec struct {
	// +optional
	Workspace *VolumeSpec `json:"workspace,omitempty"`
	// +optional
	Memory *VolumeSpec `json:"memory,omitempty"`
	// +optional
	Cache *VolumeSpec `json:"cache,omitempty"`
}

// VolumeSpec is one mounted volume.
type VolumeSpec struct {
	// Type is "pvc" (default) or "emptyDir".
	// +kubebuilder:validation:Enum=pvc;emptyDir
	// +kubebuilder:default=pvc
	// +optional
	Type string `json:"type,omitempty"`

	// Size is the requested size for pvc volumes (e.g. "10Gi").
	// +optional
	Size string `json:"size,omitempty"`

	// MountPath is where the volume is mounted in the agent pod.
	MountPath string `json:"mountPath"`
}

// SecretRef names a secret this agent may request, with its delivery spec.
type SecretRef struct {
	// Name is the kube-claw secret name (metadata in the store, value encrypted).
	Name string `json:"name"`

	// Delivery is how the secret is materialized into the pod. Defined ONCE here;
	// grants store only a content hash of it (DESIGN.md §6 DRY note).
	Delivery DeliverySpec `json:"delivery"`
}

// DeliverySpec is how a secret reaches the agent pod.
type DeliverySpec struct {
	// Type is the delivery mechanism. Only "file" (tmpfs) is supported in v0.
	// +kubebuilder:validation:Enum=file
	// +kubebuilder:default=file
	// +optional
	Type string `json:"type,omitempty"`

	// Path is the in-pod tmpfs path the secret file is written to.
	Path string `json:"path"`

	// Mode is the file mode (octal string).
	// +kubebuilder:default="0400"
	// +optional
	Mode string `json:"mode,omitempty"`

	// Env are environment variables the bootstrap sets pointing at the secret.
	// +optional
	Env map[string]string `json:"env,omitempty"`
}

// ModelSpec configures the LLM the agent runner uses.
type ModelSpec struct {
	// +optional
	ProviderRef string `json:"providerRef,omitempty"`
	// Model overrides the Anthropic model the runner's agent loop uses
	// (e.g. "claude-sonnet-5" for cheaper routine agents). Empty → the
	// runner's default (Opus).
	// +optional
	Model string `json:"model,omitempty"`
	// +optional
	SystemPrompt string `json:"systemPrompt,omitempty"`
}

// NetworkSpec restricts agent egress.
type NetworkSpec struct {
	// +optional
	EgressAllowHosts []string `json:"egressAllowHosts,omitempty"`
}

// AgentStatus is the observed state of an Agent.
type AgentStatus struct {
	// Phase is the high-level lifecycle phase.
	// +kubebuilder:validation:Enum=Sleeping;Waking;Running;Blocked;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// SelectedImageDigest is the resolved, digest-pinned image in use.
	// +optional
	SelectedImageDigest string `json:"selectedImageDigest,omitempty"`

	// AgentSpecHash binds grants to the current spec (re-approve on change).
	// +optional
	AgentSpecHash string `json:"agentSpecHash,omitempty"`

	// Conditions follow the standard Kubernetes condition convention.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ag
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.selectedImageDigest`

// Agent is the single user-facing kube-claw runtime resource.
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSpec   `json:"spec,omitempty"`
	Status AgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentList contains a list of Agent.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
}
