package connector

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Event is the payload delivered to a connector's callback URL.
type Event struct {
	RunID string `json:"runId"`
	// SessionID is the connector's ORIGINAL session id (un-namespaced).
	SessionID string `json:"sessionId,omitempty"`
	Kind      string `json:"kind"` // "output" (final answer) | "progress"
	Content   string `json:"content"`
}

// Sign computes the callback signature: hex(HMAC-SHA256(secret, ts + "." + body)).
// The timestamp is bound into the MAC so receivers can reject replays.
func Sign(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts))
	m.Write([]byte("."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// VerifySignature checks a received callback signature (for receivers written
// in Go and for tests). maxSkew bounds the timestamp age; 0 skips that check.
func VerifySignature(secret, ts, sig string, body []byte, maxSkew time.Duration) bool {
	if maxSkew > 0 {
		sec, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return false
		}
		if d := time.Since(time.Unix(sec, 0)); d > maxSkew || d < -maxSkew {
			return false
		}
	}
	return hmac.Equal([]byte(Sign(secret, ts, body)), []byte(sig))
}

// deliveryDelays are the waits before each attempt (first is immediate).
var deliveryDelays = []time.Duration{0, 2 * time.Second, 10 * time.Second}

// Deliverer pushes events to connector callback URLs. A nil HTTPClient uses a
// 10s-timeout default.
type Deliverer struct {
	HTTPClient *http.Client
	// Now is injectable for tests; nil = time.Now.
	Now func() time.Time
}

func (d *Deliverer) client() *http.Client {
	if d.HTTPClient != nil {
		return d.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Deliver POSTs ev to the connector's callback URL, signed with the
// connector's secret, retrying transient failures. Returns the last error if
// every attempt fails; outputs remain queryable via /v1/runs regardless.
func (d *Deliverer) Deliver(ctx context.Context, conn ConnectorInfo, ev Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	var lastErr error
	for _, wait := range deliveryDelays {
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if lastErr = d.attempt(ctx, conn, body, now); lastErr == nil {
			return nil
		}
	}
	return fmt.Errorf("deliver to %s: %w", conn.CallbackURL, lastErr)
}

// ConnectorInfo is the slice of a stored connector delivery needs.
type ConnectorInfo struct {
	ID            string
	CallbackURL   string
	SigningSecret string
}

func (d *Deliverer) attempt(ctx context.Context, conn ConnectorInfo, body []byte, now func() time.Time) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, conn.CallbackURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claw-Connector", conn.ID)
	req.Header.Set("X-Claw-Timestamp", ts)
	req.Header.Set("X-Claw-Signature", "v1="+Sign(conn.SigningSecret, ts, body))
	resp, err := d.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("callback returned %d", resp.StatusCode)
	}
	return nil
}
