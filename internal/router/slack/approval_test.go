package slack

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/approvals"
	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

func TestHandleApproval(t *testing.T) {
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
	sec, _ := secSvc.CreateSecret(ctx, "claw-agents", "gcp-billing", "gcp", "", []string{"U_ALEX"})
	_ = secSvc.PutValue(ctx, "claw-agents", "gcp-billing", []byte("v"), "t")
	mkReq := func(id string) {
		_ = st.Tx(ctx, func(tx store.Tx) error {
			return tx.CreateSecretRequest(store.SecretRequest{ID: id, Status: "Pending", AgentNamespace: "claw-agents", AgentName: "gcp-cost", SecretID: sec.ID, SecretName: "gcp-billing"})
		})
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clawv1alpha1.AddToScheme(scheme)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&clawv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "gcp-cost", Namespace: "claw-agents"},
		Spec:       clawv1alpha1.AgentSpec{Image: "x@sha256:abc", Secrets: []clawv1alpha1.SecretRef{{Name: "gcp-billing", Delivery: clawv1alpha1.DeliverySpec{Path: "/p"}}}},
		Status:     clawv1alpha1.AgentStatus{SelectedImageDigest: "sha256:abc", AgentSpecHash: "sha256:spec"},
	}).Build()

	r := &Router{Store: st, Approvals: &approvals.Service{Store: st, Secrets: secSvc, Reader: reader}}

	mkReq("req-1")
	// Non-granter cannot approve.
	if msg := r.HandleApproval(ctx, ActionValue("approve", "req-1"), "U_MALLORY"); !strings.Contains(msg, "not authorized") {
		t.Fatalf("non-granter msg = %q", msg)
	}
	// Granter approves.
	if msg := r.HandleApproval(ctx, ActionValue("approve", "req-1"), "U_ALEX"); !strings.Contains(msg, "approved") {
		t.Fatalf("granter msg = %q", msg)
	}
	// Deny (no granter check).
	mkReq("req-2")
	if msg := r.HandleApproval(ctx, ActionValue("deny", "req-2"), "U_X"); !strings.Contains(msg, "denied") {
		t.Fatalf("deny msg = %q", msg)
	}
	// Garbage action.
	if msg := r.HandleApproval(ctx, "garbage", "U_X"); !strings.Contains(msg, "unrecognized") {
		t.Fatalf("garbage msg = %q", msg)
	}
}
