package slack

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

func testConfig() Config {
	return Config{Routes: []Route{
		{Channels: []string{"C_COSTS"}, MentionRequired: true, AgentNamespace: "claw-agents", AgentName: "gcp-cost"},
		{Channels: []string{"C_OPEN"}, MentionRequired: false, AgentNamespace: "claw-agents", AgentName: "open-bot"},
	}}
}

func TestMatchRoute(t *testing.T) {
	c := testConfig()
	if r := c.MatchRoute("C_COSTS", true); r == nil || r.AgentName != "gcp-cost" {
		t.Errorf("mention route = %v", r)
	}
	if r := c.MatchRoute("C_COSTS", false); r != nil {
		t.Errorf("expected nil when mention required but absent, got %v", r)
	}
	if r := c.MatchRoute("C_OPEN", false); r == nil || r.AgentName != "open-bot" {
		t.Errorf("open route = %v", r)
	}
	if r := c.MatchRoute("C_OTHER", true); r != nil {
		t.Errorf("unexpected match for unknown channel: %v", r)
	}
}

func TestParseAction(t *testing.T) {
	for _, tc := range []struct {
		in              string
		wantAct, wantID string
		ok              bool
	}{
		{"approve|req-1", "approve", "req-1", true},
		{"deny|req-2", "deny", "req-2", true},
		{"bogus|req-3", "", "", false},
		{"approve", "", "", false},
	} {
		a, id, ok := ParseAction(tc.in)
		if ok != tc.ok || a != tc.wantAct || id != tc.wantID {
			t.Errorf("ParseAction(%q) = (%q,%q,%v), want (%q,%q,%v)", tc.in, a, id, ok, tc.wantAct, tc.wantID, tc.ok)
		}
	}
}

func TestHandleMessage_RouteAndDedupe(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	r := &Router{Config: testConfig(), Store: st}

	// Unmatched channel → no run.
	if id, err := r.HandleMessage(ctx, "evt-x", "C_NONE", "s", "hi", true, "U_TEST"); err != nil || id != "" {
		t.Fatalf("unmatched = %q, %v", id, err)
	}

	// Matched → run created.
	id, err := r.HandleMessage(ctx, "evt-1", "C_COSTS", "thread-1", "why did cost spike?", true, "U_TEST")
	if err != nil || id == "" {
		t.Fatalf("matched create = %q, %v", id, err)
	}
	// Duplicate event id → no second run.
	id2, err := r.HandleMessage(ctx, "evt-1", "C_COSTS", "thread-1", "again", true, "U_TEST")
	if err != nil || id2 != "" {
		t.Fatalf("dedupe = %q, %v (want empty)", id2, err)
	}

	// Exactly one run persisted.
	var n int
	_ = st.Tx(ctx, func(tx store.Tx) error {
		runs, e := tx.ListRuns(10)
		n = len(runs)
		return e
	})
	if n != 1 {
		t.Fatalf("runs persisted = %d, want 1", n)
	}
}
