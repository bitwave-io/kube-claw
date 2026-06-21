package apihttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/approvals"
	"github.com/traego/kube-claw/internal/identity"
	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

type stubIdentity struct {
	p   identity.Principal
	err error
}

func (s stubIdentity) Verify(context.Context, string) (identity.Principal, error) { return s.p, s.err }

func testAgent() *clawv1alpha1.Agent {
	return &clawv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "gcp-cost", Namespace: "claw-agents"},
		Spec: clawv1alpha1.AgentSpec{
			Image:   "x@sha256:abc",
			Secrets: []clawv1alpha1.SecretRef{{Name: "gcp-billing", Delivery: clawv1alpha1.DeliverySpec{Path: "/p", Mode: "0400"}}},
		},
		Status: clawv1alpha1.AgentStatus{SelectedImageDigest: "sha256:abc", AgentSpecHash: "sha256:spec"},
	}
}

// fullServer wires every dependency; seed adds extra objects to the fake reader.
func fullServer(t *testing.T, seed ...client.Object) *Server {
	t.Helper()
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

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clawv1alpha1.AddToScheme(scheme)
	objs := append([]client.Object{testAgent()}, seed...)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	signer, _ := identity.NewSigner()
	return &Server{
		Store: st, Reader: reader, Secrets: secSvc, Signer: signer, UIBase: "http://ui",
		Approvals: &approvals.Service{Store: st, Secrets: secSvc, Reader: reader},
	}
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

func TestSecretAndApprovalHandlers(t *testing.T) {
	s := fullServer(t)
	h := s.handler()
	ctx := context.Background()

	// create secret → one-time link
	rr := do(t, h, "POST", "/v1/secrets", `{"namespace":"claw-agents","name":"gcp-billing","type":"gcp","granters":["U_ALEX"]}`)
	if rr.Code != 201 {
		t.Fatalf("createSecret = %d (%s)", rr.Code, rr.Body)
	}
	var created map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	if !strings.HasPrefix(created["intakeURL"], "http://ui/ui/secret-intake/") {
		t.Fatalf("intakeURL = %q", created["intakeURL"])
	}

	// break-glass put value
	if rr := do(t, h, "PUT", "/v1/secrets/gcp-billing/versions?namespace=claw-agents", "SECRETDATA"); rr.Code != 200 {
		t.Fatalf("putSecretVersion = %d (%s)", rr.Code, rr.Body)
	}

	// metadata (no value)
	rr = do(t, h, "GET", "/v1/secrets/gcp-billing/metadata?namespace=claw-agents", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "U_ALEX") || strings.Contains(rr.Body.String(), "SECRETDATA") {
		t.Fatalf("metadata = %d body=%s", rr.Code, rr.Body)
	}

	// a pending request to approve
	var secID string
	_ = s.Store.Tx(ctx, func(tx store.Tx) error {
		sec, _ := tx.GetSecret("claw-agents", "gcp-billing")
		secID = sec.ID
		return tx.CreateSecretRequest(store.SecretRequest{
			ID: "req-1", Status: "Pending", AgentNamespace: "claw-agents", AgentName: "gcp-cost",
			SecretID: secID, SecretName: "gcp-billing",
		})
	})

	// list pending requests
	rr = do(t, h, "GET", "/v1/secret-requests?status=Pending", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "req-1") {
		t.Fatalf("listRequests = %d body=%s", rr.Code, rr.Body)
	}

	// approve → grant
	rr = do(t, h, "POST", "/v1/secret-requests/req-1/approve", `{"approver":"alex"}`)
	if rr.Code != 200 {
		t.Fatalf("approve = %d (%s)", rr.Code, rr.Body)
	}
	var ap map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &ap)
	if ap["grant"] == "" {
		t.Fatalf("no grant id: %v", ap)
	}

	// list grants
	rr = do(t, h, "GET", "/v1/secret-grants?namespace=claw-agents&agent=gcp-cost", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), ap["grant"]) {
		t.Fatalf("listGrants = %d body=%s", rr.Code, rr.Body)
	}

	// revoke grant
	if rr := do(t, h, "POST", "/v1/secret-grants/"+ap["grant"]+"/revoke", `{"reason":"rotating"}`); rr.Code != 200 {
		t.Fatalf("revoke = %d (%s)", rr.Code, rr.Body)
	}

	// approve unknown request → 404
	if rr := do(t, h, "POST", "/v1/secret-requests/nope/approve", `{}`); rr.Code != 404 {
		t.Fatalf("approve unknown = %d, want 404", rr.Code)
	}

	// deny path (fresh request)
	_ = s.Store.Tx(ctx, func(tx store.Tx) error {
		return tx.CreateSecretRequest(store.SecretRequest{ID: "req-2", Status: "Pending", AgentNamespace: "claw-agents", AgentName: "gcp-cost", SecretID: secID, SecretName: "gcp-billing"})
	})
	if rr := do(t, h, "POST", "/v1/secret-requests/req-2/deny", `{"reason":"no"}`); rr.Code != 200 {
		t.Fatalf("deny = %d (%s)", rr.Code, rr.Body)
	}
}

func TestPostOutputAndGetRun(t *testing.T) {
	s := fullServer(t)
	h := s.handler()
	_ = s.Store.Tx(context.Background(), func(tx store.Tx) error {
		return tx.CreateRun(store.Run{ID: "run-1", AgentNamespace: "claw-agents", AgentName: "gcp-cost", Phase: "Running"})
	})

	// runner posts an output → run marked Succeeded.
	if rr := do(t, h, "POST", "/v1/runs/run-1/outputs", `{"kind":"text","content":"hello world"}`); rr.Code != 200 {
		t.Fatalf("postOutput = %d (%s)", rr.Code, rr.Body)
	}
	// GET shows the output + Succeeded.
	rr := do(t, h, "GET", "/v1/runs/run-1", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "hello world") || !strings.Contains(rr.Body.String(), "Succeeded") {
		t.Fatalf("getRun = %d body=%s", rr.Code, rr.Body)
	}
	// output to unknown run → 404.
	if rr := do(t, h, "POST", "/v1/runs/nope/outputs", `{"content":"x"}`); rr.Code != 404 {
		t.Fatalf("postOutput unknown = %d, want 404", rr.Code)
	}
}

func TestLoginHandler(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "run-1-pod", Namespace: "claw-agents", UID: types.UID("uid-1"),
			Labels: map[string]string{"claw.run/run-id": "run-1"},
		},
		Spec: corev1.PodSpec{ServiceAccountName: "claw-agent-gcp-cost"},
	}
	s := fullServer(t, pod)
	s.Identity = stubIdentity{p: identity.Principal{
		ServiceAccount: "system:serviceaccount:claw-agents:claw-agent-gcp-cost",
		Namespace:      "claw-agents", SAName: "claw-agent-gcp-cost",
		PodName:        "run-1-pod", PodUID: "uid-1",
	}}
	h := s.handler()

	_ = s.Store.Tx(context.Background(), func(tx store.Tx) error {
		return tx.CreateRun(store.Run{ID: "run-1", AgentNamespace: "claw-agents", AgentName: "gcp-cost", Phase: "Running"})
	})

	// Happy path: valid attestation → session token.
	rr := do(t, h, "POST", "/v1/login", `{"token":"x","runId":"run-1"}`)
	if rr.Code != 200 {
		t.Fatalf("login = %d (%s)", rr.Code, rr.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["token"] == nil || out["token"] == "" {
		t.Fatalf("no session token: %v", out)
	}

	// Wrong pod UID (replay) → 401.
	s.Identity = stubIdentity{p: identity.Principal{Namespace: "claw-agents", SAName: "claw-agent-gcp-cost", PodName: "run-1-pod", PodUID: "WRONG"}}
	if rr := do(t, h, "POST", "/v1/login", `{"token":"x","runId":"run-1"}`); rr.Code != 401 {
		t.Fatalf("login uid mismatch = %d, want 401", rr.Code)
	}

	// Credential verification failure → 401.
	s.Identity = stubIdentity{err: context.Canceled}
	if rr := do(t, h, "POST", "/v1/login", `{"token":"x","runId":"run-1"}`); rr.Code != 401 {
		t.Fatalf("login verify fail = %d, want 401", rr.Code)
	}
}
