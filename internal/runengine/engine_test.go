package runengine

import (
	"context"
	"path/filepath"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

func TestInputText(t *testing.T) {
	if got := inputText(`{"text":"hello"}`); got != "hello" {
		t.Fatalf("inputText = %q", got)
	}
	if got := inputText(`not json`); got != "" {
		t.Fatalf("inputText(bad) = %q", got)
	}
}

// TestEngineGate covers the run engine: a run for an agent that requires a secret
// is BLOCKED (and a request is opened) until a valid grant exists, then LAUNCHED.
func TestEngineGate(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	const ns, agentName, secretName, path = "claw-agents", "gcp-cost", "gcp-billing", "/var/run/claw/secrets/x.json"
	const digest, specHash = "sha256:img", "sha256:spec"

	agent := &clawv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: agentName, Namespace: ns},
		Spec: clawv1alpha1.AgentSpec{
			Image:   "x@" + digest,
			Secrets: []clawv1alpha1.SecretRef{{Name: secretName, Delivery: clawv1alpha1.DeliverySpec{Path: path, Mode: "0400"}}},
		},
		Status: clawv1alpha1.AgentStatus{SelectedImageDigest: digest, AgentSpecHash: specHash},
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clawv1alpha1.AddToScheme(scheme)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()

	// The secret must exist for a request to reference it.
	var secretID string
	must(t, st.Tx(ctx, func(tx store.Tx) error {
		s := store.Secret{ID: "sec-1", Namespace: ns, Name: secretName}
		secretID = s.ID
		return tx.CreateSecret(s)
	}))
	// A Pending run.
	must(t, st.Tx(ctx, func(tx store.Tx) error {
		return tx.CreateRun(store.Run{ID: "run-1", AgentNamespace: ns, AgentName: agentName, Phase: "Pending"})
	}))

	eng := &Engine{Store: st, K8s: k8s, RunnerImage: "claw-runner:dev", ControllerURL: "http://c"}

	// Tick 1: no grant → run Blocked + pending request.
	must(t, eng.tick(ctx))
	assertPhase(t, st, "run-1", "Blocked")
	must(t, st.Tx(ctx, func(tx store.Tx) error {
		exists, _ := tx.PendingRequestExists(ns, agentName, secretID)
		if !exists {
			t.Fatal("expected a pending request after block")
		}
		return nil
	}))

	// Grant it (matching the exact binding).
	dHash := secrets.DeliveryHash(path, "0400", nil)
	must(t, st.Tx(ctx, func(tx store.Tx) error {
		return tx.CreateGrant(store.Grant{
			ID: "grant-1", AgentNamespace: ns, AgentName: agentName, SecretID: secretID,
			ImageDigest: digest, AgentSpecHash: specHash, DeliveryHash: dHash, ApprovedBy: "alex",
		})
	}))

	// Tick 2: grant present → run Running + Job created.
	must(t, eng.tick(ctx))
	assertPhase(t, st, "run-1", "Running")
	var job batchv1.Job
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: ns, Name: "run-1"}, &job); err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}
}

// TestReapRunning covers the job-failure watcher: a Running run whose Job has
// failed is marked Failed.
func TestReapRunning(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	must(t, st.Tx(ctx, func(tx store.Tx) error {
		return tx.CreateRun(store.Run{ID: "run-1", AgentNamespace: "claw-agents", AgentName: "gcp-cost", Phase: "Running"})
	}))

	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "claw-agents"},
		Status:     batchv1.JobStatus{Failed: 1, Active: 0},
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clawv1alpha1.AddToScheme(scheme)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(failedJob).Build()

	eng := &Engine{Store: st, K8s: k8s}
	eng.reapRunning(ctx)
	assertPhase(t, st, "run-1", "Failed")
}

// TestEngineEdgeCases covers: a required secret that doesn't exist yet (Blocked,
// no request), and a run whose agent is gone (evaluate returns without crashing).
func TestEngineEdgeCases(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	agent := &clawv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "gcp-cost", Namespace: "claw-agents"},
		Spec: clawv1alpha1.AgentSpec{
			Image:   "x@sha256:img",
			Secrets: []clawv1alpha1.SecretRef{{Name: "missing-secret", Delivery: clawv1alpha1.DeliverySpec{Path: "/p"}}},
		},
		Status: clawv1alpha1.AgentStatus{SelectedImageDigest: "sha256:img", AgentSpecHash: "sha256:spec"},
	}
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clawv1alpha1.AddToScheme(scheme)
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).Build()
	eng := &Engine{Store: st, K8s: k8s, RunnerImage: "r", ControllerURL: "http://c"}

	// Secret referenced but not created yet → Blocked.
	must(t, st.Tx(ctx, func(tx store.Tx) error {
		return tx.CreateRun(store.Run{ID: "run-1", AgentNamespace: "claw-agents", AgentName: "gcp-cost", Phase: "Pending"})
	}))
	must(t, eng.tick(ctx))
	assertPhase(t, st, "run-1", "Blocked")

	// Run whose agent doesn't exist → failed (a missing agent never becomes
	// ready, so the run must terminate rather than retry forever).
	must(t, st.Tx(ctx, func(tx store.Tx) error {
		return tx.CreateRun(store.Run{ID: "run-2", AgentNamespace: "claw-agents", AgentName: "ghost", Phase: "Pending"})
	}))
	eng.evaluate(ctx, store.Run{ID: "run-2", AgentNamespace: "claw-agents", AgentName: "ghost", Phase: "Pending"})
	assertPhase(t, st, "run-2", "Failed")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertPhase(t *testing.T, st store.Store, id, want string) {
	t.Helper()
	_ = st.Tx(context.Background(), func(tx store.Tx) error {
		r, err := tx.GetRun(id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Phase != want {
			t.Fatalf("run %s phase = %q, want %q", id, r.Phase, want)
		}
		return nil
	})
}
