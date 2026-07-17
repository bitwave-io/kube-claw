package apihttp

import (
	"context"
	"encoding/json"
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

// TestTokenRefresh covers the refresh exchange end-to-end: a pod-bound refresh
// token mints fresh access tokens only while its pod still exists and attests;
// pod deletion, UID reuse, access-as-refresh, and expiry are all rejected.
func TestTokenRefresh(t *testing.T) {
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
	_ = st.Tx(ctx, func(tx store.Tx) error {
		return tx.CreateRun(store.Run{ID: "run-1", AgentNamespace: "claw-agents", AgentName: "gcp-cost", Phase: "Running"})
	})

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clawv1alpha1.AddToScheme(scheme)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "run-1-abcde", Namespace: "claw-agents", UID: types.UID("uid-123"),
			Labels: map[string]string{"claw.run/run-id": "run-1"},
		},
		Spec: corev1.PodSpec{ServiceAccountName: "claw-agent-gcp-cost"},
	}
	agent := &clawv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "gcp-cost", Namespace: "claw-agents"}}
	readerWithPod := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, agent).Build()
	readerNoPod := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()

	signer, _ := identity.NewSigner()
	srv := &Server{Store: st, Reader: readerWithPod, Signer: signer}
	h := srv.handler()

	do := func(h http.Handler, token string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/token/refresh", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(rr, req)
		return rr
	}

	refresh, _ := signer.IssueRefresh("run-1", "run-1-abcde", "uid-123", time.Hour)

	// Happy path: pod exists and attests → 200 with a verifiable access token.
	rr := do(h, refresh)
	if rr.Code != http.StatusOK {
		t.Fatalf("refresh with live pod = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out.Token == "" || out.ExpiresAt == 0 {
		t.Fatalf("bad refresh response: %s", rr.Body.String())
	}
	if c, err := signer.Verify(out.Token); err != nil || c.RunID != "run-1" || c.Kind != identity.KindAccess {
		t.Fatalf("minted token does not verify as access for run-1: %+v %v", c, err)
	}

	// The pod is gone → the same refresh token must stop working.
	srvNoPod := &Server{Store: st, Reader: readerNoPod, Signer: signer}
	if rr := do(srvNoPod.handler(), refresh); rr.Code != http.StatusUnauthorized {
		t.Fatalf("refresh after pod deletion = %d, want 401", rr.Code)
	}

	// Same pod name, different UID (name reuse) → rejected.
	badUID, _ := signer.IssueRefresh("run-1", "run-1-abcde", "uid-OTHER", time.Hour)
	if rr := do(h, badUID); rr.Code != http.StatusUnauthorized {
		t.Fatalf("refresh with stale pod uid = %d, want 401", rr.Code)
	}

	// An ACCESS token must not drive the refresh exchange.
	access, _ := signer.Issue("run-1", nil, time.Hour)
	if rr := do(h, access); rr.Code != http.StatusUnauthorized {
		t.Fatalf("access token accepted by refresh = %d, want 401", rr.Code)
	}

	// Expired refresh token → 401.
	expired, _ := signer.IssueRefresh("run-1", "run-1-abcde", "uid-123", -time.Second)
	if rr := do(h, expired); rr.Code != http.StatusUnauthorized {
		t.Fatalf("expired refresh = %d, want 401", rr.Code)
	}
}
