// Package workloads builds the Kubernetes objects that execute agent runs.
//
// Phase 5 (demo slice): a one-shot Job runs the generic claw-runner, which posts
// a response back to the controller. The full path (agent image + claw-bootstrap
// + /login + secret materialization) layers on top in later work — this proves
// the trigger→pod→response loop without secrets.
package workloads

import (
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/traego/kube-claw/internal/store"
)

func mustQty(s string) resource.Quantity { return resource.MustParse(s) }

// RunJobName is the deterministic Job name for a run (idempotent creation).
func RunJobName(run store.Run) string { return run.ID }

// BuildRunJob builds the one-shot Job for a run. It runs as the agent's
// ServiceAccount (claw-agent-<name>) with a locked-down pod security context.
func BuildRunJob(run store.Run, runnerImage, controllerURL, inputText, systemPrompt, anthropicSecret, idleTimeout string) *batchv1.Job {
	if idleTimeout == "" {
		idleTimeout = "5m"
	}
	env := []corev1.EnvVar{
		{Name: "CLAW_RUN_ID", Value: run.ID},
		{Name: "CLAW_AGENT_NAME", Value: run.AgentName},
		{Name: "CLAW_AGENT_NAMESPACE", Value: run.AgentNamespace},
		{Name: "CLAW_SESSION_ID", Value: run.SessionID},
		{Name: "CLAW_CONTROLLER_URL", Value: controllerURL},
		{Name: "CLAW_INPUT", Value: inputText},
		{Name: "CLAW_SYSTEM_PROMPT", Value: systemPrompt},
		// Warm-session idle timeout: the pod waits this long for a follow-up turn
		// after answering, resetting on each new turn, before scaling to zero.
		{Name: "CLAW_IDLE_TIMEOUT", Value: idleTimeout},
		{Name: "CLAW_SECRETS_DIR", Value: "/var/run/claw/secrets"},
		{Name: "CLAW_SA_TOKEN_FILE", Value: "/var/run/claw/sa-token/token"},
		{Name: "HOME", Value: "/workspace"},
	}
	// Anthropic key is platform infrastructure injected into every run pod (not a
	// PAM secret). Optional so the Job still runs where the key isn't installed.
	if anthropicSecret != "" {
		env = append(env, corev1.EnvVar{Name: "ANTHROPIC_API_KEY", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: anthropicSecret},
				Key:                  "api-key", Optional: ptr(true),
			},
		}})
	}
	return buildJob(run, runnerImage, env)
}

func buildJob(run store.Run, runnerImage string, env []corev1.EnvVar) *batchv1.Job {
	backoff := int32(1)
	ttl := int32(600)
	// Max session lifetime cap (the runner's idle timeout is the real control;
	// this is a hard ceiling so a wedged pod can't live forever).
	deadline := int64(1800)
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "kube-claw",
		"claw.run/agent":               run.AgentName,
		"claw.run/run-id":              run.ID,
	}
	// Session label lets the engine find the warm pod for a Slack thread and
	// avoid launching a second one for follow-up turns.
	if run.SessionID != "" {
		labels["claw.run/session"] = SessionLabel(run.SessionID)
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RunJobName(run),
			Namespace: run.AgentNamespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: "claw-agent-" + run.AgentName,
					RestartPolicy:      corev1.RestartPolicyNever,
					// Run as the non-root agent user (uid 65532). FSGroup makes the
					// EmptyDir volumes (incl. the tmpfs secrets dir) group-owned by
					// that user so the runner can write materialized secrets into
					// them — without it, the volumes are root-owned and the non-root
					// process gets "permission denied" writing a fetched secret.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr(true),
						RunAsUser:      ptr(int64(65532)),
						RunAsGroup:     ptr(int64(65532)),
						FSGroup:        ptr(int64(65532)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "agent",
						Image: runnerImage,
						// bootstrap performs /login + materialize, then execs the runner.
						Command: []string{"/claw/bootstrap", "/claw/runner"},
						Env:     env,
						// The pod is the security boundary (non-root, dropped caps,
						// seccomp, NetworkPolicy, ephemeral). The rootfs is writable so
						// the agent's bash tool can install tooling into its home at
						// runtime (pip --user, ~/.local/bin, …); apt still needs root
						// and is unavailable.
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false),
							ReadOnlyRootFilesystem:   ptr(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    mustQty("100m"),
								corev1.ResourceMemory: mustQty("128Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    mustQty("500m"),
								corev1.ResourceMemory: mustQty("256Mi"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "claw-secrets", MountPath: "/var/run/claw/secrets"},
							{Name: "sa-token", MountPath: "/var/run/claw/sa-token", ReadOnly: true},
							{Name: "workspace", MountPath: "/workspace"},
							{Name: "tmp", MountPath: "/tmp"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "claw-secrets", VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
						}},
						{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "sa-token", VolumeSource: corev1.VolumeSource{
							Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
								ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
									Audience:          "claw-controller",
									ExpirationSeconds: ptr(int64(3600)),
									Path:              "token",
								},
							}}},
						}},
					},
				},
			},
		},
	}
}

func ptr[T any](v T) *T { return &v }

// SessionLabel makes a Slack thread ts a valid k8s label value (dots → dashes,
// capped at 63 chars). The engine and the Job builder must agree on this.
func SessionLabel(sessionID string) string {
	s := strings.ReplaceAll(sessionID, ".", "-")
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}
