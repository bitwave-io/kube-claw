package slack

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

// listSession returns a session's runs (oldest first, up to 10).
func listSession(ctx context.Context, st *sqlite.Store, session string) ([]store.Run, error) {
	var runs []store.Run
	err := st.Tx(ctx, func(tx store.Tx) error {
		var e error
		runs, e = tx.ListRunsBySession(session, 10)
		return e
	})
	return runs, err
}

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

// A thread that exists BECAUSE the bot was addressed (its root message is the
// engagement: eventID == thread ts) is the bot's own conversation: every reply
// creates a run without consulting the LLM gate, except a reply that OPENS
// with someone else's @mention.
func TestOwnThreadRespondsWithoutGate(t *testing.T) {
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
	r := &Router{
		Store:      st,
		ThreadGate: func(_ context.Context, _ string) bool { gateCalls++; return false },
	}
	_ = r.HandleOnboard(ctx, onboardValue("C1", "claw-agents", "assistant", true, true))

	// Own thread: the engaging mention IS the thread root (eventID == threadTS).
	const threadTS = "3333.4444"
	if runID, err := r.HandleMessage(ctx, threadTS, "C1", threadTS, "<@U1>: check GCP please", true, "U1"); err != nil || runID == "" {
		t.Fatalf("seeding own thread failed: run=%q err=%v", runID, err)
	}

	// A plain reply runs — even with a gate that would say NO (it is not asked).
	runID, err := r.HandleThreadReply(ctx, "evt-o1", "C1", threadTS, "<@U2>: what about staging?", false, "U2")
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Fatal("own-thread reply must create a run without the gate")
	}
	// A mid-text mention of another person is still for the bot.
	runID, err = r.HandleThreadReply(ctx, "evt-o2", "C1", threadTS, "<@U2>: try what <@U0PAT> suggested above", false, "U2")
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Fatal("own-thread reply with a mid-text mention must create a run")
	}
	// A reply that OPENS with someone else's mention is that person's.
	runID, err = r.HandleThreadReply(ctx, "evt-o3", "C1", threadTS, "<@U2>: <@U0PAT> can you sanity-check this?", false, "U2")
	if err != nil {
		t.Fatal(err)
	}
	if runID != "" {
		t.Fatalf("a leading @mention of another user must be dropped, got run %q", runID)
	}
	if gateCalls != 0 {
		t.Fatalf("the LLM gate must never be consulted in an own thread (calls=%d)", gateCalls)
	}

	// Follow-up runs carry the origin forward.
	runs, err := listSession(ctx, st, threadTS)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if !strings.Contains(run.Source, `"thread_origin":"own"`) {
			t.Fatalf("run %s lost the own-thread origin: %s", run.ID, run.Source)
		}
	}
}

// A joined thread (bot summoned mid-thread) keeps the LLM gate, but a mid-text
// mention of another person now REACHES the gate instead of being dropped
// outright — only a LEADING mention skips deterministically.
func TestJoinedThreadMidTextMentionReachesGate(t *testing.T) {
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
	r := &Router{
		Store:      st,
		ThreadGate: func(_ context.Context, _ string) bool { gateCalls++; return true },
	}
	_ = r.HandleOnboard(ctx, onboardValue("C1", "claw-agents", "assistant", true, true))

	// Joined: the engaging mention is a reply deep in an existing thread
	// (eventID != threadTS).
	const threadTS = "5555.6666"
	if runID, err := r.HandleMessage(ctx, "5555.9999", "C1", threadTS, "<@U1>: can you help here?", true, "U1"); err != nil || runID == "" {
		t.Fatalf("seeding joined thread failed: run=%q err=%v", runID, err)
	}

	// Mid-text mention → consult the gate (previously dropped pre-gate).
	runID, err := r.HandleThreadReply(ctx, "evt-j1", "C1", threadTS, "<@U2>: run the fix <@U0PAT> proposed", false, "U2")
	if err != nil {
		t.Fatal(err)
	}
	if runID == "" || gateCalls != 1 {
		t.Fatalf("mid-text mention must reach the gate and run on YES (run=%q, gateCalls=%d)", runID, gateCalls)
	}
	// Leading mention → still deterministic skip, gate untouched.
	runID, err = r.HandleThreadReply(ctx, "evt-j2", "C1", threadTS, "<@U2>: <@U0PAT> your call", false, "U2")
	if err != nil {
		t.Fatal(err)
	}
	if runID != "" || gateCalls != 1 {
		t.Fatalf("leading mention must be dropped without the gate (run=%q, gateCalls=%d)", runID, gateCalls)
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
