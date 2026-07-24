package apihttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/identity"
	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

// A warm pod logs in for its first run, then claims a LATER turn in the same
// session (a different run). claim-next must hand back an access token scoped to
// the CLAIMED run — otherwise the pod keeps using the login-run token and every
// run-scoped call on the follow-up turn (e.g. request_secret) 401s,
// since those endpoints require an exact run-id match.
func TestClaimNextTurn_RescopesTokenToClaimedRun(t *testing.T) {
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Two runs in the same session: run-1 is the login run (already Running), run-2
	// is the pending follow-up turn the pod will claim.
	if err := st.Tx(context.Background(), func(tx store.Tx) error {
		if e := tx.CreateRun(store.Run{ID: "run-1", AgentNamespace: "claw-agents", AgentName: "gcp-cost",
			SessionID: "sess-1", Phase: "Running"}); e != nil {
			return e
		}
		return tx.CreateRun(store.Run{ID: "run-2", AgentNamespace: "claw-agents", AgentName: "gcp-cost",
			SessionID: "sess-1", Phase: "Pending", Input: `{"text":"now pull the gitlab token"}`})
	}); err != nil {
		t.Fatalf("seed runs: %v", err)
	}

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clawv1alpha1.AddToScheme(scheme)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&clawv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "gcp-cost", Namespace: "claw-agents"},
	}).Build()

	signer, _ := identity.NewSigner()
	srv := &Server{Store: st, Reader: reader, Signer: signer}
	h := srv.handler()

	// The pod calls claim-next with its LOGIN-run token (run-1) — this is exactly
	// the stale-token situation. The endpoint is session-scoped so the call itself
	// is authorized; the response should re-scope the token to run-2.
	loginTok, _ := signer.Issue("run-1", nil, accessTokenTTL)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sess-1/claim-next?pod=run-1-abcde", nil)
	req.Header.Set("Authorization", "Bearer "+loginTok)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("claim-next = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var out struct {
		RunID     string `json:"runId"`
		Input     string `json:"input"`
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	if out.RunID != "run-2" || out.Input != "now pull the gitlab token" {
		t.Fatalf("claimed wrong turn: runId=%q input=%q", out.RunID, out.Input)
	}
	if out.Token == "" || out.ExpiresAt == 0 {
		t.Fatalf("claim-next returned no run-scoped token (token=%q expiresAt=%d)", out.Token, out.ExpiresAt)
	}
	// The crux: the fresh token must be scoped to run-2, the run this turn will
	// make its request_secret calls against.
	c, err := signer.Verify(out.Token)
	if err != nil {
		t.Fatalf("returned token does not verify: %v", err)
	}
	if c.RunID != "run-2" {
		t.Fatalf("returned token scoped to run %q, want run-2 (the claimed turn)", c.RunID)
	}
	if c.Kind != identity.KindAccess {
		t.Fatalf("returned token kind = %v, want access", c.Kind)
	}
}
