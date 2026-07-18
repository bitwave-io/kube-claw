package slack

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

func TestOnboardingSetsDynamicRoute(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	r := &Router{Store: st}

	// No config yet → no route.
	if rt := r.resolveRoute(ctx, "C_NEW", false); rt != nil {
		t.Fatalf("expected no route before onboarding, got %+v", rt)
	}

	// Onboard "Active · threads only" for C_NEW → agent assistant.
	msg := r.HandleOnboard(ctx, onboardValue("C_NEW", "claw-agents", "assistant", false, true))
	if msg == "" {
		t.Fatal("expected a confirmation message")
	}

	// Active (mention not required) → a plain message now routes.
	rt := r.resolveRoute(ctx, "C_NEW", false)
	if rt == nil || rt.AgentName != "assistant" || rt.AgentNamespace != "claw-agents" {
		t.Fatalf("dynamic route = %+v, want assistant/claw-agents", rt)
	}

	// @-only channel should not route a non-mention message.
	_ = r.HandleOnboard(ctx, onboardValue("C_MENTION", "claw-agents", "assistant", true, true))
	if rt := r.resolveRoute(ctx, "C_MENTION", false); rt != nil {
		t.Fatalf("@-only channel routed a non-mention: %+v", rt)
	}
	if rt := r.resolveRoute(ctx, "C_MENTION", true); rt == nil {
		t.Fatal("@-only channel did not route an @mention")
	}
}

// An @mention in a channel the bot isn't in yet is dropped (no route), but must
// be replayed once the channel is onboarded — otherwise the very first message
// that summoned the bot is lost.
func TestPendingMentionReplayedAfterOnboard(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	r := &Router{Store: st}

	// @mention arrives before the channel is onboarded → no run, but stashed.
	runID, err := r.HandleMessage(ctx, "1700000000.0001", "C_NEW", "1700000000.0001", "<@BOT> hello", true, "U1")
	if err != nil {
		t.Fatal(err)
	}
	if runID != "" {
		t.Fatalf("expected no run before onboarding, got %q", runID)
	}
	if _, ok := r.pending["C_NEW"]; !ok {
		t.Fatal("expected the @mention to be stashed pending onboarding")
	}

	// Onboarding the channel should replay the stashed mention as a real run.
	_ = r.HandleOnboard(ctx, onboardValue("C_NEW", "claw-agents", "assistant", true, true))
	if _, ok := r.pending["C_NEW"]; ok {
		t.Fatal("stashed mention should be consumed after onboarding")
	}
	var runs []store.Run
	if err := st.Tx(ctx, func(tx store.Tx) error {
		var e error
		runs, e = tx.ListRunsBySession("1700000000.0001", 10)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected 1 run created from replayed mention, got %d", len(runs))
	}
	if runs[0].AgentName != "assistant" {
		t.Fatalf("replayed run agent = %q, want assistant", runs[0].AgentName)
	}
}

// EnsureChannelDefault: joining a channel applies @mention-only + thread
// replies immediately (bot usable before any onboarding click), never
// overwrites an existing choice, and requires a default agent.
func TestEnsureChannelDefault(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	r := &Router{Store: st, DefaultAgent: "general"}

	if !r.EnsureChannelDefault(ctx, "C_FRESH") {
		t.Fatal("first join must apply the default config")
	}
	var cfg store.ChannelConfig
	if err := st.Tx(ctx, func(tx store.Tx) error {
		var e error
		cfg, e = tx.GetChannelConfig("C_FRESH")
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if !cfg.MentionRequired || !cfg.ThreadOnly || cfg.AgentName != "general" {
		t.Fatalf("default config = %+v, want @mention-only + threads + general", cfg)
	}

	// The default makes the channel immediately routable for @mentions only.
	if rt := r.resolveRoute(ctx, "C_FRESH", true); rt == nil {
		t.Fatal("a mention must route immediately after the default is applied")
	}
	if rt := r.resolveRoute(ctx, "C_FRESH", false); rt != nil {
		t.Fatalf("an unmentioned message must NOT route under the default, got %+v", rt)
	}

	// Re-join (or a raced second event) must not clobber a user's choice.
	_ = r.HandleOnboard(ctx, onboardValue("C_FRESH", "claw-agents", "assistant", false, false))
	if r.EnsureChannelDefault(ctx, "C_FRESH") {
		t.Fatal("an onboarded channel must not be reset to the default")
	}
	if err := st.Tx(ctx, func(tx store.Tx) error {
		var e error
		cfg, e = tx.GetChannelConfig("C_FRESH")
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.MentionRequired || cfg.AgentName != "assistant" {
		t.Fatalf("user's onboarding choice was clobbered: %+v", cfg)
	}

	// No default agent configured → nothing stored.
	bare := &Router{Store: st}
	if bare.EnsureChannelDefault(ctx, "C_OTHER") {
		t.Fatal("no DefaultAgent → the default must not be applied")
	}
}
