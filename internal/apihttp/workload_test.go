package apihttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/identity"
	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

// attestPod is the load-bearing /login security check (DESIGN.md §9). These
// cover the rejection paths from the §18.x security matrix.
func TestAttestPod(t *testing.T) {
	run := store.Run{ID: "run-1", AgentNamespace: "claw-agents", AgentName: "gcp-cost"}
	goodPod := func() *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "run-1-abcde",
				Namespace: "claw-agents",
				UID:       types.UID("uid-123"),
				Labels:    map[string]string{"claw.run/run-id": "run-1"},
			},
			Spec: corev1.PodSpec{ServiceAccountName: "claw-agent-gcp-cost"},
		}
	}

	if r := attestPod(goodPod(), "uid-123", run); r != "" {
		t.Fatalf("happy path rejected: %s", r)
	}

	// Wrong pod UID (token bound to a different pod / replay).
	if r := attestPod(goodPod(), "uid-OTHER", run); r == "" {
		t.Error("expected rejection on uid mismatch")
	}

	// Wrong namespace.
	p := goodPod()
	p.Namespace = "default"
	if r := attestPod(p, "uid-123", run); r == "" {
		t.Error("expected rejection on namespace mismatch")
	}

	// Not labelled for this run (co-resident pod of the same agent presenting another run id).
	p = goodPod()
	p.Labels["claw.run/run-id"] = "run-OTHER"
	if r := attestPod(p, "uid-123", run); r == "" {
		t.Error("expected rejection on run-id label mismatch")
	}

	// Wrong ServiceAccount.
	p = goodPod()
	p.Spec.ServiceAccountName = "default"
	if r := attestPod(p, "uid-123", run); r == "" {
		t.Error("expected rejection on service account mismatch")
	}
}

// TestMaterialize_TokenScope covers the §18.x security item: a claw session
// token may only materialize ITS run's secrets; another run's id is rejected.
func TestMaterialize_TokenScope(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dir, "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := secrets.NewLocalCipher(filepath.Join(dir, "master.keyset"))
	secSvc := &secrets.Service{Store: st, Cipher: cipher}
	if _, err := secSvc.CreateSecret(ctx, "claw-agents", "gcp-billing", "gcp", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := secSvc.PutValue(ctx, "claw-agents", "gcp-billing", []byte("KEYDATA"), "test"); err != nil {
		t.Fatal(err)
	}
	_ = st.Tx(ctx, func(tx store.Tx) error {
		return tx.CreateRun(store.Run{ID: "run-1", AgentNamespace: "claw-agents", AgentName: "gcp-cost", Phase: "Running"})
	})

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clawv1alpha1.AddToScheme(scheme)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&clawv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "gcp-cost", Namespace: "claw-agents"},
		Spec:       clawv1alpha1.AgentSpec{Image: "x@sha256:abc", Secrets: []clawv1alpha1.SecretRef{{Name: "gcp-billing", Delivery: clawv1alpha1.DeliverySpec{Path: "/p"}}}},
	}).Build()

	signer, _ := identity.NewSigner()
	srv := &Server{Store: st, Reader: reader, Secrets: secSvc, Signer: signer}
	h := srv.handler()

	tok, _ := signer.Issue("run-1", []string{"gcp-billing"}, time.Minute)

	// Correct run + scoped token → 200.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/run-1/materialize", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("materialize own run = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}

	// Same token against a DIFFERENT run id → 401.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/runs/run-2/materialize", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("cross-run materialize = %d, want 401", rr.Code)
	}

	// Garbage token → 401.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/runs/run-1/materialize", nil)
	req.Header.Set("Authorization", "Bearer garbage")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("garbage token = %d, want 401", rr.Code)
	}
}
