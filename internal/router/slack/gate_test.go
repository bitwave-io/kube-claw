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

	// A message @mentioning someone ELSE is addressed to that person: dropped
	// deterministically, without even consulting the LLM gate.
	gateAnswer = true
	beforeOther := gateCalls
	runID, err = r.HandleMessage(ctx, "evt-other", "C_ACTIVE", "evt-other", "<@U2PERSON> can you check the LB config?", false, "U1")
	if err != nil {
		t.Fatal(err)
	}
	if runID != "" {
		t.Fatalf("a message @mentioning another user must be dropped, got run %q", runID)
	}
	if gateCalls != beforeOther {
		t.Fatalf("the gate must not be consulted when another user is @mentioned (calls went %d→%d)", beforeOther, gateCalls)
	}

	// The "<@sender>: " prefix that agentText prepends to EVERY message is the
	// speaker, not an addressee — it must NOT trip the someone-else skip
	// (regression: this prefix made the gate drop all unmentioned messages).
	gateAnswer = true
	runID, err = r.HandleMessage(ctx, "evt-prefixed", "C_ACTIVE", "evt-prefixed", "<@U1SENDER>: why is the prod log bill so high?", false, "U1SENDER")
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Fatal("a sender-prefixed message with no other mention must reach the gate and create a run")
	}
	// …but a real mention of someone else AFTER the sender prefix still skips.
	runID, err = r.HandleMessage(ctx, "evt-prefixed-other", "C_ACTIVE", "evt-prefixed-other", "<@U1SENDER>: <@U2PERSON> can you take this?", false, "U1SENDER")
	if err != nil {
		t.Fatal(err)
	}
	if runID != "" {
		t.Fatalf("a sender-prefixed message that @mentions another user must be dropped, got run %q", runID)
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

// Inside a thread the bot participates in: a reply that @mentions another user
// is addressed to that person and must be dropped deterministically (without
// consulting the LLM gate); other non-mention replies go through the (default
// open) thread gate; an @mention of the bot always proceeds.
func TestThreadReplyGate(t *testing.T) {
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
	gateAnswer := true
	r := &Router{
		Store: st,
		ThreadGate: func(_ context.Context, _ string) bool {
			gateCalls++
			return gateAnswer
		},
	}
	_ = r.HandleOnboard(ctx, onboardValue("C1", "claw-agents", "assistant", true, false))

	// Seed the thread: an @mention starts the session the bot is engaged in.
	const threadTS = "1111.2222"
	if runID, err := r.HandleMessage(ctx, "evt-root", "C1", threadTS, "<@BOT> can you check GCP?", true, "U1"); err != nil || runID == "" {
		t.Fatalf("seeding the thread failed: run=%q err=%v", runID, err)
	}

	// A reply @mentioning another user is for that person: dropped without
	// consulting the gate ("@Pat could we give it more access?" is Pat's).
	runID, err := r.HandleThreadReply(ctx, "evt-r1", "C1", threadTS, "<@U0PAT> could we give it access to other projects too?", false, "U2")
	if err != nil {
		t.Fatal(err)
	}
	if runID != "" {
		t.Fatalf("a thread reply @mentioning another user must be dropped, got run %q", runID)
	}
	if gateCalls != 0 {
		t.Fatalf("the thread gate must not be consulted when another user is @mentioned (calls=%d)", gateCalls)
	}

	// A plain follow-up consults the gate; gate=YES → run.
	runID, err = r.HandleThreadReply(ctx, "evt-r2", "C1", threadTS, "can you also check the staging cluster?", false, "U2")
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" || gateCalls != 1 {
		t.Fatalf("gate=YES should create a run (run=%q, gateCalls=%d)", runID, gateCalls)
	}

	// The sender prefix agentText prepends must not read as "someone else"
	// (regression: it made every unmentioned thread reply drop pre-gate).
	runID, err = r.HandleThreadReply(ctx, "evt-r2b", "C1", threadTS, "<@U2>: and the canary cluster too please", false, "U2")
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" || gateCalls != 2 {
		t.Fatalf("a sender-prefixed follow-up must reach the gate and create a run (run=%q, gateCalls=%d)", runID, gateCalls)
	}

	// Gate=NO → dropped.
	gateAnswer = false
	runID, err = r.HandleThreadReply(ctx, "evt-r3", "C1", threadTS, "ya of course", false, "U1")
	if err != nil {
		t.Fatal(err)
	}
	if runID != "" {
		t.Fatalf("gate=NO should drop the reply, got run %q", runID)
	}

	// An @mention of the bot always proceeds, gate not consulted.
	before := gateCalls
	runID, err = r.HandleThreadReply(ctx, "evt-r4", "C1", threadTS, "<@BOT> check your perms again", true, "U1")
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Fatal("an @mention in-thread must always create a run")
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
