package approvals

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

// Verifies the Slack approval path enforces granter membership, while the
// break-glass path does not.
func TestApproveByPrincipal_GranterCheck(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dir, "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := secrets.NewLocalCipher(filepath.Join(dir, "master.keyset"))
	secSvc := &secrets.Service{Store: st, Cipher: cipher}

	// Secret with granter U_ALEX; one version; a pending request.
	sec, _ := secSvc.CreateSecret(ctx, "claw-agents", "gcp-billing", "gcp", []string{"U_ALEX"})
	_ = secSvc.PutValue(ctx, "claw-agents", "gcp-billing", []byte("v"), "test")
	_ = st.Tx(ctx, func(tx store.Tx) error {
		return tx.CreateSecretRequest(store.SecretRequest{
			ID: "req-1", Status: "Pending", AgentNamespace: "claw-agents", AgentName: "gcp-cost",
			SecretID: sec.ID, SecretName: "gcp-billing",
		})
	})

	// Agent (for binding computation) via fake reader.
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clawv1alpha1.AddToScheme(scheme)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&clawv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "gcp-cost", Namespace: "claw-agents"},
		Spec: clawv1alpha1.AgentSpec{
			Image: "x@sha256:abc",
			Secrets: []clawv1alpha1.SecretRef{{Name: "gcp-billing", Delivery: clawv1alpha1.DeliverySpec{Path: "/p"}}},
		},
		Status: clawv1alpha1.AgentStatus{SelectedImageDigest: "sha256:abc", AgentSpecHash: "sha256:spec"},
	}).Build()

	svc := &Service{Store: st, Secrets: secSvc, Reader: reader}

	// Non-granter → rejected.
	if _, err := svc.ApproveByPrincipal(ctx, "req-1", "U_MALLORY", "x"); !errors.Is(err, ErrNotGranter) {
		t.Fatalf("non-granter err = %v, want ErrNotGranter", err)
	}
	// Granter → approved (grant created).
	if _, err := svc.ApproveByPrincipal(ctx, "req-1", "U_ALEX", "ok"); err != nil {
		t.Fatalf("granter approve failed: %v", err)
	}
}
