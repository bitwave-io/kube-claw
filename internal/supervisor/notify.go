package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// SlackNotifier is the supervisor's ONLY Slack capability: one bare
// chat.postMessage HTTPS call for failure/rollback notifications (DESIGN.md
// §24.1). Deliberately not slack-go / socket mode — the supervisor's value is
// a minimal, boring surface, and this path must work while the controller
// (which owns the real Slack connection) is down.
type SlackNotifier struct {
	token string
	url   string // overridable in tests
	hc    *http.Client
}

func NewSlackNotifier(botToken string) *SlackNotifier {
	return &SlackNotifier{
		token: botToken,
		url:   "https://slack.com/api/chat.postMessage",
		hc:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Notify posts text to a channel or user id (Slack opens the DM implicitly).
func (s *SlackNotifier) Notify(ctx context.Context, target, text string) error {
	body, _ := json.Marshal(map[string]string{"channel": target, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("slack chat.postMessage: %s", out.Error)
	}
	return nil
}
