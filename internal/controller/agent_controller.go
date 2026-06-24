// Package controller holds the kube-claw controller-runtime reconcilers.
//
// Reconcile flow (Phase 1 — DESIGN.md §17, §31):
//
//	Agent applied
//	  └─ resolve image digest (sha256 from spec.image)
//	  └─ compute agentSpecHash (binds grants; re-approve on change)
//	  └─ ensure per-Agent ServiceAccount (NO RBAC — agents get zero K8s perms)
//	  └─ ensure storage PVCs (workspace, memory)
//	  └─ ensure baseline NetworkPolicy (deny ingress to agent pods)
//	  └─ status: phase, selectedImageDigest, agentSpecHash, conditions
//
// Pod scale-to-zero lifecycle lands in Phase 5.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
)

// AgentReconciler reconciles Agent objects.
type AgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// RestrictAgentEgress, when true, adds an egress NetworkPolicy that denies
	// east/west (private CIDRs) while allowing DNS, the controller, and the public
	// internet. Off by default: it requires a CNI that enforces egress correctly
	// for host-networked DNS (NodeLocalDNS) — enable + verify per cluster.
	RestrictAgentEgress bool
}

// +kubebuilder:rbac:groups=claw.run,resources=agents,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=claw.run,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts;persistentvolumeclaims,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch

// Reconcile ensures the supporting objects and status for one Agent.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)

	var agent clawv1alpha1.Agent
	if err := r.Get(ctx, req.NamespacedName, &agent); err != nil {
		// Not found = deleted; owned objects are garbage-collected by owner refs.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Resolve the digest from an inline image. Agents may instead use
	// baseImageRef (resolved by the run engine), in which case there is no inline
	// digest to record here. The CRD CEL rule already enforces "@sha256:" on a
	// non-empty image at apply time; be defensive anyway.
	digest := ""
	if agent.Spec.Image != "" {
		d, err := imageDigest(agent.Spec.Image)
		if err != nil {
			meta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
				Type:    "Ready",
				Status:  metav1.ConditionFalse,
				Reason:  "InvalidImage",
				Message: err.Error(),
			})
			agent.Status.Phase = "Failed"
			return ctrl.Result{}, r.statusUpdate(ctx, &agent)
		}
		digest = d
	}

	if err := r.ensureServiceAccount(ctx, &agent); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure service account: %w", err)
	}
	if err := r.ensurePVCs(ctx, &agent); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure pvcs: %w", err)
	}
	if err := r.ensureNetworkPolicy(ctx, &agent); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure network policy: %w", err)
	}

	agent.Status.SelectedImageDigest = digest
	agent.Status.AgentSpecHash = specHash(&agent.Spec)
	if agent.Status.Phase == "" {
		agent.Status.Phase = "Sleeping"
	}
	meta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  metav1.ConditionTrue,
		Reason:  "ResourcesEnsured",
		Message: "ServiceAccount, storage, and network policy are present",
	})
	// SecretGrantsReady is evaluated by the secret authority (Phase 4); until then
	// it is unknown rather than asserted.
	meta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type:    "SecretGrantsReady",
		Status:  metav1.ConditionUnknown,
		Reason:  "NotEvaluated",
		Message: "secret grant evaluation lands in phase 4",
	})

	lg.Info("reconciled agent", "agent", agent.Name, "digest", digest, "phase", agent.Status.Phase)
	return ctrl.Result{}, r.statusUpdate(ctx, &agent)
}

func (r *AgentReconciler) statusUpdate(ctx context.Context, agent *clawv1alpha1.Agent) error {
	if err := r.Status().Update(ctx, agent); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	return nil
}

func (r *AgentReconciler) ensureServiceAccount(ctx context.Context, agent *clawv1alpha1.Agent) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: agentSAName(agent.Name), Namespace: agent.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		// No RBAC bindings are created: agent pods get zero Kubernetes API
		// permissions by design (DESIGN.md §29).
		sa.Labels = agentLabels(agent.Name)
		return controllerutil.SetControllerReference(agent, sa, r.Scheme)
	})
	return err
}

func (r *AgentReconciler) ensurePVCs(ctx context.Context, agent *clawv1alpha1.Agent) error {
	type vol struct {
		suffix string
		spec   *clawv1alpha1.VolumeSpec
	}
	for _, v := range []vol{
		{"workspace", agent.Spec.Storage.Workspace},
		{"memory", agent.Spec.Storage.Memory},
	} {
		if v.spec == nil || v.spec.Type == "emptyDir" {
			continue
		}
		if err := r.ensurePVC(ctx, agent, v.suffix, v.spec); err != nil {
			return err
		}
	}
	return nil
}

func (r *AgentReconciler) ensurePVC(ctx context.Context, agent *clawv1alpha1.Agent, suffix string, spec *clawv1alpha1.VolumeSpec) error {
	name := fmt.Sprintf("%s-%s", agent.Name, suffix)
	// PVC specs are largely immutable, so create-if-absent rather than CreateOrUpdate.
	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: name}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	size := spec.Size
	if size == "" {
		size = "1Gi"
	}
	qty, perr := resource.ParseQuantity(size)
	if perr != nil {
		return fmt.Errorf("pvc %s: invalid size %q: %w", name, size, perr)
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agent.Namespace, Labels: agentLabels(agent.Name)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}
	if err := controllerutil.SetControllerReference(agent, pvc, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, pvc)
}

func (r *AgentReconciler) ensureNetworkPolicy(ctx context.Context, agent *clawv1alpha1.Agent) error {
	// Baseline: deny all ingress to agent pods, and constrain egress to DNS, the
	// controller, and the public internet — denying east/west to other in-cluster
	// pods/services (private CIDRs). FQDN-pinning the internet rule to
	// spec.network.egressAllowHosts (api.anthropic.com / cloud APIs) still needs a
	// CNI with FQDN policy (Cilium / GKE Dataplane V2) — tracked as a follow-up.
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	dnsPort := intstr.FromInt(53)
	policyTypes := []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
	var egress []networkingv1.NetworkPolicyEgressRule
	if r.RestrictAgentEgress {
		policyTypes = append(policyTypes, networkingv1.PolicyTypeEgress)
		egress = []networkingv1.NetworkPolicyEgressRule{
			{ // DNS — port 53 to anywhere (covers kube-dns AND host-networked NodeLocalDNS)
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dnsPort}, {Protocol: &tcp, Port: &dnsPort}},
			},
			{ // the claw controller (login, materialize, run callbacks)
				To: []networkingv1.NetworkPolicyPeer{{NamespaceSelector: nsSelector("claw-system")}},
			},
			{ // public internet (model + cloud APIs); private ranges excluded → no east/west
				To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{
					CIDR:   "0.0.0.0/0",
					Except: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"},
				}}},
			},
		}
	}
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: agent.Name + "-agent", Namespace: agent.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = agentLabels(agent.Name)
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"claw.run/agent": agent.Name}},
			PolicyTypes: policyTypes,
			Egress:      egress,
		}
		return controllerutil.SetControllerReference(agent, np, r.Scheme)
	})
	return err
}

// nsSelector matches a namespace by its standard metadata.name label.
func nsSelector(name string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": name}}
}

// SetupWithManager wires the reconciler and the objects it owns.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&clawv1alpha1.Agent{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}

// --- helpers ---

func agentSAName(agent string) string { return "claw-agent-" + agent }

func agentLabels(agent string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "kube-claw",
		"claw.run/agent":               agent,
	}
}

// imageDigest extracts the "sha256:..." digest from a digest-pinned image ref.
func imageDigest(image string) (string, error) {
	at := strings.LastIndex(image, "@")
	if at < 0 || !strings.HasPrefix(image[at+1:], "sha256:") {
		return "", fmt.Errorf("image %q is not pinned to a sha256 digest", image)
	}
	return image[at+1:], nil
}

// specHash is a stable hash of the Agent spec, used to bind grants (re-approve
// on spec change). JSON field order is deterministic for a fixed struct.
func specHash(spec *clawv1alpha1.AgentSpec) string {
	b, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
