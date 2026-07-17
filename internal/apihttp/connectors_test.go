package apihttp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/traego/kube-claw/internal/connector"
)

// callbackSink records signed webhook deliveries for assertions.
type callbackSink struct {
	mu     sync.Mutex
	events []connector.Event
	sigOK  bool
	secret string
}

func (c *callbackSink) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sig := strings.TrimPrefix(r.Header.Get("X-Claw-Signature"), "v1=")
		ts := r.Header.Get("X-Claw-Timestamp")
		var ev connector.Event
		_ = json.Unmarshal(body, &ev)
		c.mu.Lock()
		c.sigOK = connector.VerifySignature(c.secret, ts, sig, body, time.Minute)
		c.events = append(c.events, ev)
		c.mu.Unlock()
	}
}

func (c *callbackSink) wait(t *testing.T) connector.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.events) > 0 {
			ev := c.events[0]
			c.mu.Unlock()
			return ev
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no callback delivery within deadline")
	return connector.Event{}
}

func TestConnectorLifecycle(t *testing.T) {
	sink := &callbackSink{}
	cb := httptest.NewServer(sink.handler())
	defer cb.Close()

	s := fullServer(t)
	h := s.handler()

	// Register: agent must exist (testAgent seeds claw-agents/gcp-cost).
	rr := do(t, h, "POST", "/v1/connectors",
		`{"name":"bitwave-slack","callbackUrl":"`+cb.URL+`","agent":{"name":"gcp-cost"}}`)
	if rr.Code != 201 {
		t.Fatalf("createConnector = %d (%s)", rr.Code, rr.Body)
	}
	var created map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	id, apiKey := created["id"], created["apiKey"]
	sink.secret = created["signingSecret"]
	if id == "" || !strings.HasPrefix(apiKey, "ck_") || sink.secret == "" {
		t.Fatalf("created = %v", created)
	}
	if created["ingestPath"] != "/v1/connectors/"+id+"/messages" {
		t.Fatalf("ingestPath = %q", created["ingestPath"])
	}

	// Unknown agent is rejected up front.
	if rr := do(t, h, "POST", "/v1/connectors",
		`{"name":"x","callbackUrl":"http://cb","agent":{"name":"nope"}}`); rr.Code != 400 {
		t.Fatalf("createConnector with bad agent = %d", rr.Code)
	}

	// List never leaks the key hash or signing secret.
	rr = do(t, h, "GET", "/v1/connectors", "")
	if rr.Code != 200 || strings.Contains(rr.Body.String(), sink.secret) ||
		strings.Contains(rr.Body.String(), connector.HashKey(apiKey)) {
		t.Fatalf("listConnectors = %d (%s)", rr.Code, rr.Body)
	}

	ingest := "/v1/connectors/" + id + "/messages"

	// No key / bad key / mismatched connector id → 401.
	if rr := do(t, h, "POST", ingest, `{"text":"hi"}`); rr.Code != 401 {
		t.Fatalf("ingest without key = %d", rr.Code)
	}
	if rr := doAuth(t, h, "POST", ingest, `{"text":"hi"}`, "ck_wrong"); rr.Code != 401 {
		t.Fatalf("ingest with bad key = %d", rr.Code)
	}
	if rr := doAuth(t, h, "POST", "/v1/connectors/conn-other/messages", `{"text":"hi"}`, apiKey); rr.Code != 401 {
		t.Fatalf("ingest against foreign connector id = %d", rr.Code)
	}

	// Ingest a message → run created, session accepted.
	rr = doAuth(t, h, "POST", ingest, `{"eventId":"e1","sessionId":"chat-1","text":"what can you do?","user":"u9"}`, apiKey)
	if rr.Code != 202 {
		t.Fatalf("ingest = %d (%s)", rr.Code, rr.Body)
	}
	var accepted map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &accepted)
	runID := accepted["runId"]
	if runID == "" || accepted["sessionId"] != "chat-1" {
		t.Fatalf("accepted = %v", accepted)
	}

	// Redelivery of the same event dedupes.
	if rr := doAuth(t, h, "POST", ingest, `{"eventId":"e1","sessionId":"chat-1","text":"what can you do?"}`, apiKey); rr.Code != 200 || !strings.Contains(rr.Body.String(), "duplicate") {
		t.Fatalf("duplicate ingest = %d (%s)", rr.Code, rr.Body)
	}

	// Runner posts the answer → signed webhook lands on the callback URL with
	// the connector's ORIGINAL session id.
	tok, _ := s.Signer.Issue(runID, nil, time.Minute)
	if rr := doAuth(t, h, "POST", "/v1/runs/"+runID+"/outputs", `{"kind":"text","content":"I help with GCP costs"}`, tok); rr.Code != 200 {
		t.Fatalf("postOutput = %d (%s)", rr.Code, rr.Body)
	}
	ev := sink.wait(t)
	if !sink.sigOK {
		t.Fatal("callback signature did not verify")
	}
	if ev.RunID != runID || ev.SessionID != "chat-1" || ev.Kind != "output" || ev.Content != "I help with GCP costs" {
		t.Fatalf("delivered event = %+v", ev)
	}

	// Rotate: the old key dies in the same transaction the new one is born.
	rr = do(t, h, "POST", "/v1/connectors/"+id+"/rotate-key", "")
	if rr.Code != 200 {
		t.Fatalf("rotate = %d (%s)", rr.Code, rr.Body)
	}
	var rotated map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &rotated)
	if rr := doAuth(t, h, "POST", ingest, `{"eventId":"e2","text":"hi"}`, apiKey); rr.Code != 401 {
		t.Fatalf("old key after rotate = %d", rr.Code)
	}
	if rr := doAuth(t, h, "POST", ingest, `{"eventId":"e2","text":"hi"}`, rotated["apiKey"]); rr.Code != 202 {
		t.Fatalf("new key after rotate = %d (%s)", rr.Code, rr.Body)
	}

	// Delete → key stops working.
	if rr := do(t, h, "DELETE", "/v1/connectors/"+id, ""); rr.Code != 200 {
		t.Fatalf("delete = %d (%s)", rr.Code, rr.Body)
	}
	if rr := doAuth(t, h, "POST", ingest, `{"eventId":"e3","text":"hi"}`, rotated["apiKey"]); rr.Code != 401 {
		t.Fatalf("ingest after delete = %d", rr.Code)
	}
}

func TestConnectorManagementRequiresAdmin(t *testing.T) {
	s := fullServer(t)
	s.AdminPassword = "hunter2"
	h := s.handler()

	if rr := do(t, h, "POST", "/v1/connectors", `{"name":"x","callbackUrl":"http://cb","agent":{"name":"gcp-cost"}}`); rr.Code != 401 {
		t.Fatalf("create without admin auth = %d", rr.Code)
	}
	if rr := do(t, h, "GET", "/v1/connectors", ""); rr.Code != 401 {
		t.Fatalf("list without admin auth = %d", rr.Code)
	}

	r := httptest.NewRequest("POST", "/v1/connectors",
		strings.NewReader(`{"name":"x","callbackUrl":"http://cb","agent":{"name":"gcp-cost"}}`))
	r.SetBasicAuth("admin", "hunter2")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != 201 {
		t.Fatalf("create with admin auth = %d (%s)", rr.Code, rr.Body)
	}
}
