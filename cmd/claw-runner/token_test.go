package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// setTokens swaps the process-wide token state for a test and restores it after.
func setTokens(t *testing.T, token, refresh string, expiresAt time.Time) {
	t.Helper()
	clawTokens.mu.Lock()
	prevToken, prevRefresh, prevExp := clawTokens.token, clawTokens.refresh, clawTokens.expiresAt
	clawTokens.token, clawTokens.refresh, clawTokens.expiresAt = token, refresh, expiresAt
	clawTokens.mu.Unlock()
	t.Cleanup(func() {
		clawTokens.mu.Lock()
		clawTokens.token, clawTokens.refresh, clawTokens.expiresAt = prevToken, prevRefresh, prevExp
		clawTokens.mu.Unlock()
	})
}

// tokenJSON is the controller's login/refresh response shape.
func tokenJSON(tok, refresh string) string {
	b, _ := json.Marshal(map[string]any{
		"token": tok, "refreshToken": refresh,
		"expiresAt": time.Now().Add(30 * time.Minute).Unix(),
	})
	return string(b)
}

// A token close to expiry is renewed via /v1/token/refresh before the request.
func TestClawToken_ProactiveRefresh(t *testing.T) {
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/token/refresh" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer refresh-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		refreshCalls.Add(1)
		_, _ = w.Write([]byte(tokenJSON("tok-2", "")))
	}))
	defer srv.Close()

	// 5m left < 10m margin → must refresh.
	setTokens(t, "tok-1", "refresh-1", time.Now().Add(5*time.Minute))
	if got := clawToken(srv.URL); got != "tok-2" {
		t.Fatalf("clawToken = %q, want refreshed tok-2", got)
	}
	// Fresh 30m token → no second refresh.
	if got := clawToken(srv.URL); got != "tok-2" || refreshCalls.Load() != 1 {
		t.Fatalf("expected exactly one refresh (got %d, token %q)", refreshCalls.Load(), got)
	}
}

// A 401 on any authenticated call triggers renew-and-retry-once via authedDo —
// the controller-restart case, where the token is dead before its expiry.
func TestAuthedDo_RetriesOnceAfter401(t *testing.T) {
	var apiCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/token/refresh", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(tokenJSON("tok-good", "")))
	})
	mux.HandleFunc("/v1/runs/run-1/progress", func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer tok-good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("CLAW_CONTROLLER_URL", srv.URL)

	setTokens(t, "tok-stale", "refresh-1", time.Now().Add(30*time.Minute)) // not near expiry
	resp, err := authedDo(context.Background(), http.MethodPost, srv.URL+"/v1/runs/run-1/progress", []byte(`{"text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authedDo final status = %d, want 200 after renew+retry", resp.StatusCode)
	}
	if apiCalls.Load() != 2 {
		t.Fatalf("expected 401 then retried 200 (2 calls), got %d", apiCalls.Load())
	}
}

// When the refresh token itself is rejected (controller restarted → new signing
// key), the runner re-runs the full /login with the projected SA token.
func TestRenewal_FallsBackToSALogin(t *testing.T) {
	var loggedIn atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/token/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // signer key rotated
	})
	mux.HandleFunc("/v1/login", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Token, RunID string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Token != "sa-token-bytes" || req.RunID != "run-1" {
			t.Errorf("login got token=%q runID=%q", req.Token, req.RunID)
		}
		loggedIn.Store(true)
		_, _ = w.Write([]byte(tokenJSON("tok-relogin", "refresh-2")))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	saFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(saFile, []byte("sa-token-bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAW_SA_TOKEN_FILE", saFile)
	t.Setenv("CLAW_RUN_ID", "run-1")

	setTokens(t, "tok-dead", "refresh-dead", time.Now().Add(30*time.Minute))
	if got := forceTokenRenewal(srv.URL); got != "tok-relogin" {
		t.Fatalf("forceTokenRenewal = %q, want tok-relogin via SA re-login", got)
	}
	if !loggedIn.Load() {
		t.Fatal("expected a full /login after refresh rejection")
	}
	clawTokens.mu.Lock()
	gotRefresh := clawTokens.refresh
	clawTokens.mu.Unlock()
	if gotRefresh != "refresh-2" {
		t.Fatalf("re-login must install the new refresh token, got %q", gotRefresh)
	}
}
