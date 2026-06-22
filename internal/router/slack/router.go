// Package slack is the Slack connector: it turns Slack messages into AgentRuns
// and handles interactive PAM approvals over Socket Mode (DESIGN.md §8.1, §12).
//
// Socket Mode (outbound WebSocket) means there is NO per-message HMAC signature
// (that exists only on the HTTP Events API); the trust is the authenticated
// app-token connection plus the granter check on approvals. The live transport
// needs real Slack tokens to exercise; the routing/dedupe/approval logic here is
// unit-tested independently.
package slack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/traego/kube-claw/internal/approvals"
	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
)

// Route maps a Slack channel (with optional mention requirement) to an agent.
type Route struct {
	Channels        []string `json:"channels"`
	MentionRequired bool     `json:"mentionRequired"`
	AgentNamespace  string   `json:"agentNamespace"`
	AgentName       string   `json:"agentName"`
}

// Config is the connector configuration (from Helm values / controller config).
type Config struct {
	Routes []Route
}

// MatchRoute returns the first route matching the channel + mention state, or nil.
func (c Config) MatchRoute(channel string, mentioned bool) *Route {
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.MentionRequired && !mentioned {
			continue
		}
		for _, ch := range r.Channels {
			if ch == channel {
				return r
			}
		}
	}
	return nil
}

// Router ties Slack events to runs + approvals. (Transport is wired in socket.go.)
type Router struct {
	Config       Config
	Store        store.Store
	Approvals    *approvals.Service
	Secrets      *secrets.Service // for DM-based secret registration
	Notifier     *Notifier        // for DM replies
	UIBase       string           // intake link base URL
	BotUserID    string           // this bot's Slack user id (set at connect)
	DefaultAgent string           // agent assigned when a channel is onboarded
	AgentsNS     string           // namespace for onboarded agents (default claw-agents)
	Classifier   *Classifier      // LLM image router (nil = use the agent's own image)
}

// pickImage runs the classifier (if configured) against the registered base
// images and returns the chosen base-image ref ("" = none/default).
func (r *Router) pickImage(ctx context.Context, request string) string {
	if r.Classifier == nil {
		return ""
	}
	var imgs []store.BaseImage
	_ = r.Store.Tx(ctx, func(tx store.Tx) error {
		got, e := tx.ListBaseImages()
		imgs = got
		return e
	})
	return r.Classifier.PickImage(ctx, request, imgs)
}

// resolveRoute matches a channel to an agent: static Helm routes first, then a
// dynamic channel config set via the onboarding flow.
func (r *Router) resolveRoute(ctx context.Context, channel string, mentioned bool) *Route {
	if rt := r.Config.MatchRoute(channel, mentioned); rt != nil {
		return rt
	}
	var cfg store.ChannelConfig
	found := false
	_ = r.Store.Tx(ctx, func(tx store.Tx) error {
		c, e := tx.GetChannelConfig(channel)
		if e == nil {
			cfg, found = c, true
		}
		return nil
	})
	if !found || (cfg.MentionRequired && !mentioned) {
		return nil
	}
	return &Route{Channels: []string{channel}, MentionRequired: cfg.MentionRequired,
		AgentNamespace: cfg.AgentNamespace, AgentName: cfg.AgentName}
}

// onboardValue encodes a preset button: "onboard|<channel>|<agentNs>|<agent>|<mention>|<thread>".
func onboardValue(channel, ns, agent string, mention, thread bool) string {
	return fmt.Sprintf("onboard|%s|%s|%s|%d|%d", channel, ns, agent, b2i(mention), b2i(thread))
}

func (r *Router) agentsNS() string {
	if r.AgentsNS != "" {
		return r.AgentsNS
	}
	return "claw-agents"
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// HandleOnboard applies an onboarding choice (a channel preset) and returns a
// confirmation message. Value format from onboardValue.
func (r *Router) HandleOnboard(ctx context.Context, value string) string {
	parts := strings.Split(value, "|")
	if len(parts) != 6 || parts[0] != "onboard" {
		return "couldn't read that choice"
	}
	channel, ns, agent := parts[1], parts[2], parts[3]
	mention, thread := parts[4] == "1", parts[5] == "1"
	if err := r.Store.Tx(ctx, func(tx store.Tx) error {
		return tx.SetChannelConfig(store.ChannelConfig{
			Channel: channel, AgentNamespace: ns, AgentName: agent,
			MentionRequired: mention, ThreadOnly: thread,
		})
	}); err != nil {
		return "couldn't save: " + err.Error()
	}
	watch := "every message"
	if mention {
		watch = "only when @mentioned"
	}
	where := "in the channel"
	if thread {
		where = "in threads only"
	}
	return fmt.Sprintf("Got it — in <#%s> I'll respond to *%s* and post *%s*, handled by agent `%s`.", channel, watch, where, agent)
}

// HandleDM handles a direct message to the bot. Today it supports secret
// registration: "register secret <name> [description]" mints a one-time intake
// link with the DMing user as the secret's granter. Returns the reply text.
func (r *Router) HandleDM(ctx context.Context, userID, text string) string {
	low := strings.ToLower(text)
	if !strings.Contains(low, "register") || !strings.Contains(low, "secret") {
		return "Hi! DM me `register secret <name> [description]` and I'll send a one-time link to add the value — you'll be set as its approver."
	}
	if r.Secrets == nil {
		return "secret registration isn't configured on this controller"
	}
	name, desc := parseRegisterSecret(text)
	if name == "" {
		return "What should the secret be called? DM me `register secret <name> [description]`."
	}
	const ns = "claw-agents"
	// Create (ignore already-exists) with the DMing user as granter, then mint a link.
	_, _ = r.Secrets.CreateSecret(ctx, ns, name, "", desc, []string{userID})
	tok, err := r.Secrets.MintIntakeToken(ctx, ns, name)
	if err != nil {
		return "couldn't create the intake link: " + err.Error()
	}
	return fmt.Sprintf("Open this one-time link to add the value for *%s* (you, <@%s>, are the approver):\n%s/ui/secret-intake/%s",
		name, userID, r.UIBase, tok)
}

// parseRegisterSecret pulls the name (and optional description) following the
// word "secret" in a DM like "register secret gcp-billing read-only key".
func parseRegisterSecret(text string) (name, description string) {
	fields := strings.Fields(text)
	for i, f := range fields {
		if strings.EqualFold(f, "secret") && i+1 < len(fields) {
			name = fields[i+1]
			if i+2 < len(fields) {
				description = strings.Join(fields[i+2:], " ")
			}
			return name, description
		}
	}
	return "", ""
}

// HandleMessage dedupes a Slack event and, if it matches a route, creates a run.
// Returns the new run id ("" if deduped or unmatched).
func (r *Router) HandleMessage(ctx context.Context, eventID, channel, sessionID, text string, mentioned bool) (string, error) {
	route := r.resolveRoute(ctx, channel, mentioned)
	if route == nil {
		return "", nil
	}
	// New session → let the router pick the best tool image for this request.
	image := r.pickImage(ctx, text)
	runID := "run-" + randHex()
	created := false
	err := r.Store.Tx(ctx, func(tx store.Tx) error {
		dup, err := tx.SeenEvent("slack", eventID)
		if err != nil || dup {
			return err
		}
		if err := tx.CreateRun(store.Run{
			ID: runID, AgentNamespace: route.AgentNamespace, AgentName: route.AgentName,
			SessionID: sessionID, Phase: "Pending",
			Source: fmt.Sprintf(`{"trigger":"slack","channel":%q,"event":%q,"image":%q}`, channel, eventID, image),
			Input:  fmt.Sprintf(`{"text":%q}`, text),
		}); err != nil {
			return err
		}
		created = true
		return tx.AppendAudit(store.AuditEvent{Type: "connector.event_received", RunID: runID, Actor: "slack"})
	})
	if err != nil || !created {
		return "", err
	}
	return runID, nil
}

// HandleThreadReply continues a conversation: a reply in a thread the bot is
// already engaged in creates a follow-up run for the same agent (no @mention
// needed). Returns "" if the thread has no prior bot run (so we ignore it).
func (r *Router) HandleThreadReply(ctx context.Context, eventID, channel, threadTS, text string) (string, error) {
	var prior store.Run
	found := false
	if err := r.Store.Tx(ctx, func(tx store.Tx) error {
		runs, e := tx.ListRunsBySession(threadTS, 1)
		if e != nil {
			return e
		}
		if len(runs) > 0 {
			prior, found = runs[0], true
		}
		return nil
	}); err != nil {
		return "", err
	}
	if !found {
		return "", nil // not a thread the bot started
	}
	runID := "run-" + randHex()
	created := false
	err := r.Store.Tx(ctx, func(tx store.Tx) error {
		dup, err := tx.SeenEvent("slack", eventID)
		if err != nil || dup {
			return err
		}
		if err := tx.CreateRun(store.Run{
			ID: runID, AgentNamespace: prior.AgentNamespace, AgentName: prior.AgentName,
			SessionID: threadTS, Phase: "Pending",
			Source: fmt.Sprintf(`{"trigger":"slack","channel":%q,"event":%q}`, channel, eventID),
			Input:  fmt.Sprintf(`{"text":%q}`, text),
		}); err != nil {
			return err
		}
		created = true
		return tx.AppendAudit(store.AuditEvent{Type: "connector.thread_reply", RunID: runID, Actor: "slack"})
	})
	if err != nil || !created {
		return "", err
	}
	return runID, nil
}

// ActionValue encodes an approve/deny button payload: "approve|<reqID>".
func ActionValue(action, reqID string) string { return action + "|" + reqID }

// ParseAction splits an action value into (action, reqID).
func ParseAction(v string) (action, reqID string, ok bool) {
	parts := strings.SplitN(v, "|", 2)
	if len(parts) != 2 || (parts[0] != "approve" && parts[0] != "deny") {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// HandleApproval processes an interactive approve/deny click. The clicking user
// must be a granter (enforced by approvals.ApproveByPrincipal). Returns a short
// status message to post back to Slack.
func (r *Router) HandleApproval(ctx context.Context, actionValue, slackUserID string) string {
	action, reqID, ok := ParseAction(actionValue)
	if !ok {
		return "unrecognized action"
	}
	switch action {
	case "approve":
		if _, err := r.Approvals.ApproveByPrincipal(ctx, reqID, slackUserID, "approved via Slack"); err != nil {
			if err == approvals.ErrNotGranter {
				return fmt.Sprintf("<@%s> is not authorized to approve this secret", slackUserID)
			}
			return "approval failed: " + err.Error()
		}
		return "approved ✅"
	case "deny":
		if err := r.Approvals.Deny(ctx, reqID, slackUserID, "denied via Slack"); err != nil {
			return "deny failed: " + err.Error()
		}
		return "denied"
	}
	return "unrecognized action"
}

func randHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
