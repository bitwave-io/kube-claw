package supervisor

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
)

// StatefulSetName / ServiceName are fixed: the Helm-rendered Service,
// NetworkPolicy, and Ingress select these labels/names, and existing installs'
// data PVC is named data-claw-controller-0 — the ≤0.3.x adoption path depends
// on recreating the same identity (DESIGN.md §24.8).
const (
	StatefulSetName = "claw-controller"
	appLabel        = "claw-controller"
)

// BuildStatefulSet renders the controller StatefulSet from the ControlPlane
// spec — a faithful transcription of the retired Helm template
// (controller-statefulset.yaml ≤ chart 0.3.x), embedded here so the shape is
// versioned with the supervisor binary. Chart-level shape changes therefore
// require a supervisor release (requiresHelmUpgrade — DESIGN.md §24.3).
func BuildStatefulSet(cp *clawv1alpha1.ControlPlane, des Desired) *appsv1.StatefulSet {
	c := cp.Spec.Controller
	ports := cp.Spec.Service

	dataDir := stringOr(c.DataDir, "/var/lib/claw")
	logFormat := stringOr(c.LogFormat, "console")
	selfURL := stringOr(c.SelfURL, fmt.Sprintf("http://claw-controller.%s.svc:%d", cp.Namespace, portOr(ports.APIPort, 8443)))
	uiBaseURL := stringOr(c.UIBaseURL, "http://localhost:8090")
	adminSecret := stringOr(c.AdminSecretName, "claw-admin")
	enableRouter := true
	if c.EnableRouter != nil {
		enableRouter = *c.EnableRouter
	}
	storageSize := stringOr(c.Storage.Size, "20Gi")

	args := []string{
		fmt.Sprintf("--data-dir=%s", dataDir),
		fmt.Sprintf("--enable-router=%t", enableRouter),
		fmt.Sprintf("--health-probe-bind-address=:%d", portOr(ports.ProbePort, 8081)),
		fmt.Sprintf("--api-bind-address=:%d", portOr(ports.APIPort, 8443)),
		fmt.Sprintf("--ui-bind-address=:%d", portOr(ports.UIPort, 8090)),
		fmt.Sprintf("--ui-base-url=%s", uiBaseURL),
		fmt.Sprintf("--runner-image=%s", des.RunnerImage),
		fmt.Sprintf("--self-url=%s", selfURL),
		fmt.Sprintf("--log-format=%s", logFormat),
	}
	if c.Artifacts != nil {
		args = append(args,
			fmt.Sprintf("--artifact-ttl=%s", stringOr(c.Artifacts.TTL, "24h")),
			fmt.Sprintf("--artifact-max-ttl=%s", stringOr(c.Artifacts.MaxTTL, "168h")),
		)
	}

	env := []corev1.EnvVar{
		{Name: "CLAW_RESTRICT_AGENT_EGRESS", Value: fmt.Sprintf("%t", c.RestrictAgentEgress)},
		{Name: "CLAW_ADMIN_PASSWORD", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: adminSecret},
				Key:                  "password",
				Optional:             ptr.To(true),
			},
		}},
		// The controller finds its ControlPlane CR to write runningVersion
		// (startup-confirmed) and approval annotations (DESIGN.md §24.2).
		{Name: "CLAW_CONTROLPLANE_NAME", Value: cp.Name},
		{Name: "CLAW_CONTROLPLANE_NAMESPACE", Value: cp.Namespace},
	}
	if cp.Spec.Slack.RoutesJSON != "" {
		env = append(env, corev1.EnvVar{Name: "CLAW_SLACK_ROUTES", Value: cp.Spec.Slack.RoutesJSON})
	}
	if cp.Spec.Slack.Enabled {
		tokenSecret := stringOr(cp.Spec.Slack.TokenSecretName, "claw-slack-tokens")
		env = append(env,
			corev1.EnvVar{Name: "CLAW_SLACK_APP_TOKEN", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: tokenSecret},
					Key:                  "app-token",
				},
			}},
			corev1.EnvVar{Name: "CLAW_SLACK_BOT_TOKEN", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: tokenSecret},
					Key:                  "bot-token",
				},
			}},
			corev1.EnvVar{Name: "ANTHROPIC_API_KEY", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "claw-anthropic-key"},
					Key:                  "api-key",
					Optional:             ptr.To(true),
				},
			}},
		)
	}

	labels := map[string]string{"app.kubernetes.io/name": appLabel}
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(storageSize)},
			},
		},
	}
	if c.Storage.StorageClassName != "" {
		pvc.Spec.StorageClassName = ptr.To(c.Storage.StorageClassName)
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      StatefulSetName,
			Namespace: cp.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: StatefulSetName,
			Replicas:    ptr.To(replicasOr(c.Replicas, 1)),
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: "claw-controller",
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						RunAsUser:    ptr.To(int64(65532)),
						RunAsGroup:   ptr.To(int64(65532)),
						// fsGroup makes the PD group-writable by 65532 so the
						// controller can create its SQLite DB (see chart note).
						FSGroup:             ptr.To(int64(65532)),
						FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
						SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:            "controller",
						Image:           des.ControllerImage,
						ImagePullPolicy: pullPolicyOr(cp.Spec.Image.PullPolicy),
						Args:            args,
						Env:             env,
						Ports: []corev1.ContainerPort{
							{Name: "api", ContainerPort: portOr(ports.APIPort, 8443)},
							{Name: "ui", ContainerPort: portOr(ports.UIPort, 8090)},
							{Name: "metrics", ContainerPort: portOr(ports.MetricsPort, 8080)},
							{Name: "probe", ContainerPort: portOr(ports.ProbePort, 8081)},
						},
						LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("probe")},
						}},
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromString("probe")},
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						Resources: c.Resources,
						VolumeMounts: []corev1.VolumeMount{
							{Name: "data", MountPath: dataDir},
						},
					}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{pvc},
		},
	}
}

func stringOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func portOr(v, def int32) int32 {
	if v != 0 {
		return v
	}
	return def
}

func replicasOr(v *int32, def int32) int32 {
	if v != nil {
		return *v
	}
	return def
}

func pullPolicyOr(p corev1.PullPolicy) corev1.PullPolicy {
	if p != "" {
		return p
	}
	return corev1.PullIfNotPresent
}
