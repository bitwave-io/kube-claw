// Claw session token management for the runner. The bootstrap does the initial
// /login and hands us CLAW_TOKEN / CLAW_REFRESH_TOKEN / CLAW_TOKEN_EXPIRES_AT;
// from then on the runner keeps the access token fresh itself so a warm session
// can outlive any token TTL:
//
//  1. proactive refresh (POST /v1/token/refresh) when the access token is close
//     to expiry, and
//  2. full re-login with the projected SA token (kubelet keeps it rotated) when
//     the refresh token itself is rejected — e.g. the controller restarted and
//     regenerated its signing key.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// refreshMargin is how long before expiry we renew. Warm pods poll every 3s, so
// any margin comfortably above one poll interval guarantees a renewal attempt
// lands (with room to retry) before the old token dies.
const refreshMargin = 10 * time.Minute

// authedDo sends an authenticated request to the controller and, on a 401,
// renews the session token and retries ONCE. Expiry is handled proactively by
// clawToken, but a controller restart invalidates tokens that our clock says
// are still good — every authenticated call must recover from that, not just
// claim-next and the final output post. The body is a byte slice (not a
// Reader) so the retry can resend it. The caller owns resp.Body.
func authedDo(ctx context.Context, method, url string, body []byte) (*http.Response, error) {
	controllerURL := os.Getenv("CLAW_CONTROLLER_URL")
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if len(body) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		authClawToken(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			fmt.Fprintf(os.Stderr, "claw-runner: %s %s unauthorized — renewing session token and retrying\n", method, url)
			forceTokenRenewal(controllerURL)
			continue
		}
		return resp, nil
	}
}

type tokenState struct {
	mu        sync.Mutex
	token     string
	refresh   string
	expiresAt time.Time
}

var clawTokens = func() *tokenState {
	ts := &tokenState{
		token:   os.Getenv("CLAW_TOKEN"),
		refresh: os.Getenv("CLAW_REFRESH_TOKEN"),
	}
	if v, err := strconv.ParseInt(os.Getenv("CLAW_TOKEN_EXPIRES_AT"), 10, 64); err == nil && v > 0 {
		ts.expiresAt = time.Unix(v, 0)
	}
	return ts
}()

// clawToken returns the current access token, renewing it first if it is close
// to expiry. Renewal failure is logged and the stale token returned — the
// caller's request will 401 and forceTokenRenewal gives it a retry path.
func clawToken(controllerURL string) string {
	clawTokens.mu.Lock()
	defer clawTokens.mu.Unlock()
	// A pre-refresh-era controller sends no expiry; nothing to manage then.
	if !clawTokens.expiresAt.IsZero() && time.Until(clawTokens.expiresAt) < refreshMargin {
		if err := clawTokens.renewLocked(controllerURL); err != nil {
			fmt.Fprintf(os.Stderr, "claw-runner: token renewal failed: %v\n", err)
		}
	}
	return clawTokens.token
}

// forceTokenRenewal renews immediately (called after a 401 — the token is dead
// regardless of what our clock thinks). Returns the new token, or "" if renewal
// failed.
func forceTokenRenewal(controllerURL string) string {
	clawTokens.mu.Lock()
	defer clawTokens.mu.Unlock()
	if err := clawTokens.renewLocked(controllerURL); err != nil {
		fmt.Fprintf(os.Stderr, "claw-runner: token renewal failed: %v\n", err)
		return ""
	}
	return clawTokens.token
}

// renewLocked tries the refresh exchange, then falls back to a full re-login.
// Callers hold ts.mu.
func (ts *tokenState) renewLocked(controllerURL string) error {
	if ts.refresh != "" {
		if err := ts.doRefreshLocked(controllerURL); err == nil {
			return nil
		} else {
			fmt.Fprintf(os.Stderr, "claw-runner: token refresh failed (%v) — attempting full re-login\n", err)
		}
	}
	return ts.doLoginLocked(controllerURL)
}

type tokenResp struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
}

func (ts *tokenState) doRefreshLocked(controllerURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, controllerURL+"/v1/token/refresh", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+ts.refresh)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("controller returned %s", resp.Status)
	}
	var out tokenResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		return fmt.Errorf("bad refresh response")
	}
	ts.applyLocked(out)
	fmt.Printf("claw-runner: access token refreshed (valid until %s)\n", ts.expiresAt.Format(time.RFC3339))
	return nil
}

// doLoginLocked re-runs the bootstrap's /login exchange with the projected SA
// token (re-read from disk — kubelet rotates it well within its 1h lifetime).
func (ts *tokenState) doLoginLocked(controllerURL string) error {
	runID := os.Getenv("CLAW_RUN_ID")
	tokenFile := os.Getenv("CLAW_SA_TOKEN_FILE")
	if tokenFile == "" {
		tokenFile = "/var/run/claw/sa-token/token"
	}
	sa, err := os.ReadFile(tokenFile)
	if err != nil {
		return fmt.Errorf("read SA token: %w", err)
	}
	body, _ := json.Marshal(map[string]string{"token": string(bytes.TrimSpace(sa)), "runId": runID})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, controllerURL+"/v1/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("re-login: controller returned %s", resp.Status)
	}
	var out tokenResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		return fmt.Errorf("bad login response")
	}
	ts.applyLocked(out)
	fmt.Printf("claw-runner: re-logged in (token valid until %s)\n", ts.expiresAt.Format(time.RFC3339))
	return nil
}

func (ts *tokenState) applyLocked(out tokenResp) {
	ts.token = out.Token
	if out.RefreshToken != "" {
		ts.refresh = out.RefreshToken
	}
	if out.ExpiresAt > 0 {
		ts.expiresAt = time.Unix(out.ExpiresAt, 0)
	}
}

// installClaimedToken swaps in the run-scoped access token that claim-next hands
// back with each warm turn. The access token minted at /login is bound to the
// login run; without this, run-scoped calls on a follow-up turn (a different run)
// 401 — and renewal can't help, since /token/refresh re-issues for the SAME bound
// run. The refresh token is left untouched (it stays pod-bound and is not
// re-issued here).
func installClaimedToken(token string, expiresAt int64) {
	if token == "" {
		return
	}
	clawTokens.mu.Lock()
	defer clawTokens.mu.Unlock()
	clawTokens.applyLocked(tokenResp{Token: token, ExpiresAt: expiresAt})
}
