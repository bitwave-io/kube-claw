package slack

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/traego/kube-claw/internal/store/sqlite"
)

// In an active-participant channel (mentionRequired=false), an unprompted message
// is only turned into a run when the relevance gate says yes; an @mention always
// bypasses the gate.
func TestRelevanceGate(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	var gateCalls int
	gateAnswer := false
	r := &Router{
		Store: st,
		RelevanceGate: func(_ context.Context, _ string) bool {
			gateCalls++
			return gateAnswer
		},
	}
	// Active channel: respond to every message (mentionRequired=false).
	_ = r.HandleOnboard(ctx, onboardValue("C_ACTIVE", "claw-agents", "assistant", false, false))

	// Gate says NO → unprompted message is dropped, no run.
	runID, err := r.HandleMessage(ctx, "evt1", "C_ACTIVE", "evt1", "lol nice", false, "U1")
	if err != nil {
		t.Fatal(err)
	}
	if runID != "" {
		t.Fatalf("gate=NO should drop the message, got run %q", runID)
	}
	if gateCalls != 1 {
		t.Fatalf("expected gate consulted once, got %d", gateCalls)
	}

	// Gate says YES → a run is created.
	gateAnswer = true
	runID, err = r.HandleMessage(ctx, "evt2", "C_ACTIVE", "evt2", "why is the prod log bill so high?", false, "U1")
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Fatal("gate=YES should create a run")
	}

	// An @mention bypasses the gate entirely (even with gate=NO it must respond).
	gateAnswer = false
	before := gateCalls
	runID, err = r.HandleMessage(ctx, "evt3", "C_ACTIVE", "evt3", "<@BOT> help", true, "U1")
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Fatal("an @mention must always create a run, regardless of the gate")
	}
	if gateCalls != before {
		t.Fatalf("the gate must not be consulted for an @mention (calls went %d→%d)", before, gateCalls)
	}
}

// With no Classifier and no RelevanceGate injected, the gate is open — routed
// messages respond as before (preserves pre-gate behavior for LLM-less setups).
func TestRelevanceGateOpenWithoutClassifier(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	r := &Router{Store: st} // no Classifier, no RelevanceGate
	_ = r.HandleOnboard(ctx, onboardValue("C_ACTIVE", "claw-agents", "assistant", false, false))

	runID, err := r.HandleMessage(ctx, "evt1", "C_ACTIVE", "evt1", "anything", false, "U1")
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Fatal("with no gate configured, a routed message should still create a run")
	}
}
