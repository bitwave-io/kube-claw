package connector

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/store/sqlite"
)

func TestKeysAndSessionNamespacing(t *testing.T) {
	key, hash := NewAPIKey()
	if len(key) < 20 || key[:3] != "ck_" {
		t.Fatalf("key = %q", key)
	}
	if HashKey(key) != hash {
		t.Fatal("hash mismatch")
	}
	if HashKey("other") == hash {
		t.Fatal("hash collision")
	}

	if got := SessionKey("conn-1", "thread-9"); got != "conn-1:thread-9" {
		t.Fatalf("SessionKey = %q", got)
	}
	if got := SessionKey("conn-1", ""); got != "" {
		t.Fatalf("empty session should stay empty, got %q", got)
	}
	if got := ExternalSessionID("conn-1", "conn-1:thread-9"); got != "thread-9" {
		t.Fatalf("ExternalSessionID = %q", got)
	}
}

func TestSignVerify(t *testing.T) {
	body := []byte(`{"runId":"run-1"}`)
	sig := Sign("secret", "1700000000", body)
	if !VerifySignature("secret", "1700000000", sig, body, 0) {
		t.Fatal("valid signature rejected")
	}
	if VerifySignature("wrong", "1700000000", sig, body, 0) {
		t.Fatal("bad secret accepted")
	}
	if VerifySignature("secret", "1700000001", sig, body, 0) {
		t.Fatal("timestamp not bound into MAC")
	}
	if VerifySignature("secret", "1700000000", sig, body, time.Minute) {
		t.Fatal("stale timestamp accepted despite skew bound")
	}
}

func TestDeliverRetriesAndSigns(t *testing.T) {
	orig := deliveryDelays
	deliveryDelays = []time.Duration{0, 5 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { deliveryDelays = orig })

	var calls atomic.Int32
	var gotSig, gotTS, gotConn string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway) // transient failure → retry
			return
		}
		gotSig = r.Header.Get("X-Claw-Signature")
		gotTS = r.Header.Get("X-Claw-Timestamp")
		gotConn = r.Header.Get("X-Claw-Connector")
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()

	d := &Deliverer{}
	info := ConnectorInfo{ID: "conn-1", CallbackURL: srv.URL, SigningSecret: "cs_test"}
	err := d.Deliver(context.Background(), info, Event{RunID: "run-1", SessionID: "s1", Kind: "output", Content: "hi"})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (one retry)", calls.Load())
	}
	if gotConn != "conn-1" {
		t.Fatalf("connector header = %q", gotConn)
	}
	if len(gotSig) < 4 || gotSig[:3] != "v1=" {
		t.Fatalf("signature header = %q", gotSig)
	}
	if !VerifySignature("cs_test", gotTS, gotSig[3:], gotBody, 0) {
		t.Fatal("delivered signature does not verify")
	}
}

func TestDeliverGivesUpAfterRetries(t *testing.T) {
	orig := deliveryDelays
	deliveryDelays = []time.Duration{0, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { deliveryDelays = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := &Deliverer{}
	err := d.Deliver(context.Background(), ConnectorInfo{ID: "c", CallbackURL: srv.URL, SigningSecret: "s"}, Event{RunID: "r"})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
}

func openStore(t *testing.T) store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "claw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestIngest(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	conn := store.Connector{ID: "conn-1", Name: "test", CallbackURL: "http://cb",
		APIKeyHash: "h", SigningSecret: "s", AgentNamespace: "claw-agents", AgentName: "general"}

	// New session → pinned agent, namespaced session id.
	runID, err := Ingest(ctx, st, conn, Message{EventID: "e1", SessionID: "chat-7", Text: "hello", User: "u1"})
	if err != nil || runID == "" {
		t.Fatalf("Ingest = %q, %v", runID, err)
	}
	var run store.Run
	_ = st.Tx(ctx, func(tx store.Tx) error {
		r, e := tx.GetRun(runID)
		run = r
		return e
	})
	if run.SessionID != "conn-1:chat-7" {
		t.Fatalf("SessionID = %q, want namespaced", run.SessionID)
	}
	if run.AgentName != "general" || run.Phase != "Pending" {
		t.Fatalf("run = %+v", run)
	}
	if SourceConnectorID(run.Source) != "conn-1" {
		t.Fatalf("SourceConnectorID(%q) failed", run.Source)
	}

	// Duplicate event → no new run.
	if dupID, err := Ingest(ctx, st, conn, Message{EventID: "e1", SessionID: "chat-7", Text: "hello"}); err != nil || dupID != "" {
		t.Fatalf("duplicate Ingest = %q, %v", dupID, err)
	}

	// Follow-up in the same session reuses the session's agent even if the
	// connector's pinned agent has changed since.
	conn2 := conn
	conn2.AgentName = "different-agent"
	followID, err := Ingest(ctx, st, conn2, Message{EventID: "e2", SessionID: "chat-7", Text: "again"})
	if err != nil || followID == "" {
		t.Fatalf("follow-up Ingest = %q, %v", followID, err)
	}
	_ = st.Tx(ctx, func(tx store.Tx) error {
		r, e := tx.GetRun(followID)
		run = r
		return e
	})
	if run.AgentName != "general" {
		t.Fatalf("follow-up agent = %q, want session continuity", run.AgentName)
	}
}
