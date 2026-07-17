package apihttp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/traego/kube-claw/internal/artifacts"
	"github.com/traego/kube-claw/internal/store"
)

// TestPublishArtifactAndShareLink walks the full share flow: the runner callback
// publishes a doc, the returned public link serves raw markdown on the UI
// listener, and a reshare mints a fresh link while killing the old one.
func TestPublishArtifactAndShareLink(t *testing.T) {
	s := fullServer(t)
	s.Artifacts = &artifacts.Service{Store: s.Store}
	h := s.handler()
	ctx := context.Background()

	if err := s.Store.Tx(ctx, func(tx store.Tx) error {
		return tx.CreateRun(store.Run{ID: "run-1", AgentNamespace: "claw-agents", AgentName: "gcp-cost",
			SessionID: "th-1", Phase: "Running"})
	}); err != nil {
		t.Fatal(err)
	}
	tok, _ := s.Signer.Issue("run-1", nil, time.Hour)

	// No session token → 401.
	if rr := do(t, h, "POST", "/v1/runs/run-1/artifacts", `{"title":"D","content":"x"}`); rr.Code != 401 {
		t.Fatalf("unauthenticated publish = %d", rr.Code)
	}

	rr := doAuth(t, h, "POST", "/v1/runs/run-1/artifacts", `{"title":"Design","content":"# Doc\nbody"}`, tok)
	if rr.Code != 201 {
		t.Fatalf("publish = %d (%s)", rr.Code, rr.Body)
	}
	var pub map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &pub)
	if !strings.HasPrefix(pub["url"], "http://ui/d/") || pub["artifactId"] == "" || pub["expiresAt"] == "" {
		t.Fatalf("publish response = %v", pub)
	}

	// The public link serves the raw markdown from the separate UI listener.
	ui := (&UIServer{Artifacts: s.Artifacts}).handler()
	get := func(url string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		ui.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
		return rec
	}
	path := strings.TrimPrefix(pub["url"], "http://ui")
	if rec := get(path); rec.Code != 200 || rec.Body.String() != "# Doc\nbody" ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/markdown") {
		t.Fatalf("share GET = %d %q (%s)", rec.Code, rec.Body, rec.Header().Get("Content-Type"))
	}
	// Multi-read within the TTL.
	if rec := get(path); rec.Code != 200 {
		t.Fatalf("second share GET = %d", rec.Code)
	}
	if rec := get("/d/not-a-token"); rec.Code != 404 {
		t.Fatalf("unknown token GET = %d", rec.Code)
	}

	// Reshare (content-free) → fresh link; the old one is revoked → 410.
	rr = doAuth(t, h, "POST", "/v1/runs/run-1/artifacts", `{"artifactId":"`+pub["artifactId"]+`","title":"Design"}`, tok)
	if rr.Code != 201 {
		t.Fatalf("reshare = %d (%s)", rr.Code, rr.Body)
	}
	var re map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &re)
	if re["url"] == pub["url"] || re["artifactId"] != pub["artifactId"] {
		t.Fatalf("reshare response = %v", re)
	}
	if rec := get(path); rec.Code != 410 || !strings.Contains(rec.Body.String(), "reshare") {
		t.Fatalf("old link after reshare = %d (%s)", rec.Code, rec.Body)
	}
	if rec := get(strings.TrimPrefix(re["url"], "http://ui")); rec.Code != 200 {
		t.Fatalf("new link = %d", rec.Code)
	}

	// Resharing an artifact from a different session reads as 404.
	if err := s.Store.Tx(ctx, func(tx store.Tx) error {
		return tx.CreateRun(store.Run{ID: "run-9", AgentNamespace: "claw-agents", AgentName: "gcp-cost",
			SessionID: "th-OTHER", Phase: "Running"})
	}); err != nil {
		t.Fatal(err)
	}
	tok9, _ := s.Signer.Issue("run-9", nil, time.Hour)
	if rr := doAuth(t, h, "POST", "/v1/runs/run-9/artifacts",
		`{"artifactId":"`+pub["artifactId"]+`","title":"Design"}`, tok9); rr.Code != 404 {
		t.Fatalf("cross-session reshare = %d (%s)", rr.Code, rr.Body)
	}
}
