package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
)

// Notifier posts back to Slack: run replies (#2) and interactive PAM approval
// requests (#3). Constructed with the bot token; nil when Slack isn't configured.
// The live posting needs real Slack credentials to exercise.
type Notifier struct {
	api *slack.Client

	nameMu sync.Mutex
	names  map[string]nameEntry
}

func NewNotifier(botToken string) *Notifier {
	return &Notifier{api: slack.New(botToken)}
}

// --- Slack id → name resolution (dashboard display) -------------------------

// nameTTL bounds how long id→name lookups are cached. Failures are cached too
// (negative caching): a Slack outage must not turn every dashboard load into a
// wall of timed-out API calls.
const nameTTL = time.Hour

type nameEntry struct {
	name string
	at   time.Time
}

// UserName resolves a Slack user id to a human name ("" when Slack is off or
// the lookup fails — callers fall back to the raw id). Nil-receiver safe.
func (n *Notifier) UserName(ctx context.Context, id string) string {
	return n.resolveName(ctx, "u:"+id, func(ctx context.Context) (string, error) {
		u, err := n.api.GetUserInfoContext(ctx, id)
		if err != nil {
			return "", err
		}
		switch {
		case u.Profile.DisplayName != "":
			return u.Profile.DisplayName, nil
		case u.RealName != "":
			return u.RealName, nil
		}
		return u.Name, nil
	})
}

// ChannelName resolves a channel id to its name (no leading '#'), "" on
// failure. Nil-receiver safe.
func (n *Notifier) ChannelName(ctx context.Context, id string) string {
	return n.resolveName(ctx, "c:"+id, func(ctx context.Context) (string, error) {
		ch, err := n.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: id})
		if err != nil {
			return "", err
		}
		return ch.Name, nil
	})
}

func (n *Notifier) resolveName(ctx context.Context, key string, fetch func(context.Context) (string, error)) string {
	if n == nil || strings.HasSuffix(key, ":") { // nil notifier or empty id
		return ""
	}
	n.nameMu.Lock()
	if e, ok := n.names[key]; ok && time.Since(e.at) < nameTTL {
		n.nameMu.Unlock()
		return e.name
	}
	n.nameMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	name, err := fetch(ctx)
	if err != nil {
		name = ""
	}
	n.nameMu.Lock()
	if n.names == nil {
		n.names = map[string]nameEntry{}
	}
	n.names[key] = nameEntry{name: name, at: time.Now()}
	n.nameMu.Unlock()
	return name
}

// PostReply posts an agent's output back to the originating Slack thread.
func (n *Notifier) PostReply(ctx context.Context, channel, threadTS, text string) error {
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, _, err := n.api.PostMessageContext(ctx, channel, opts...)
	return err
}

// PostApproval posts an interactive approve/deny message for a blocked run. The
// button values carry the request id (parsed back by HandleApproval); only a
// configured granter's click is honored.
func (n *Notifier) PostApproval(ctx context.Context, channel, threadTS, secretName, reqID string) error {
	section := slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn",
			fmt.Sprintf(":lock: An agent run needs secret *%s*. Approve?", secretName), false, false),
		nil, nil)
	approve := slack.NewButtonBlockElement("approve", ActionValue("approve", reqID),
		slack.NewTextBlockObject("plain_text", "Approve", false, false)).WithStyle(slack.StylePrimary)
	deny := slack.NewButtonBlockElement("deny", ActionValue("deny", reqID),
		slack.NewTextBlockObject("plain_text", "Deny", false, false)).WithStyle(slack.StyleDanger)
	actions := slack.NewActionBlock("claw-approval", approve, deny)

	opts := []slack.MsgOption{slack.MsgOptionBlocks(section, actions)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, _, err := n.api.PostMessageContext(ctx, channel, opts...)
	return err
}

// SlackChannel extracts the channel from a run's Source JSON (set by
// HandleMessage as {"trigger":"slack","channel":"...","event":"..."}).
func SlackChannel(source string) string { return slackSource(source).Channel }

// SlackEventTS extracts the triggering message ts from a run's Source JSON.
func SlackEventTS(source string) string { return slackSource(source).Event }

// SlackUser extracts the requesting user's Slack id from a run's Source JSON.
func SlackUser(source string) string { return slackSource(source).User }

// SlackIsDM reports whether the run came from a direct message — its SessionID
// is the IM channel id (one continuous conversation), NOT a thread ts, so
// replies must post without thread targeting.
func SlackIsDM(source string) bool { return slackSource(source).DM }

type slackSrc struct {
	Trigger string `json:"trigger"`
	Channel string `json:"channel"`
	Event   string `json:"event"`
	User    string `json:"user"`
	DM      bool   `json:"dm"`
}

func slackSource(source string) slackSrc {
	var s slackSrc
	if json.Unmarshal([]byte(source), &s) != nil || s.Trigger != "slack" {
		return slackSrc{}
	}
	return s
}

// adminClaimValue is the interaction value of the "make me the upgrade admin"
// onboarding button. The claiming user is the clicker (from the callback), so
// the value carries no payload.
const adminClaimValue = "adminclaim"

// PostOnboarding DMs the inviter (or posts in-channel) asking how the bot should
// behave in a channel it was just added to: active vs @-only, and in-channel vs
// threads-only. Each button stores a channel config when clicked. offerAdmin
// additionally offers the upgrade-admin role (shown only while no admin is set,
// DESIGN.md §24.6).
func (n *Notifier) PostOnboarding(ctx context.Context, target, channel, ns, agent string, offerAdmin bool) error {
	intro := slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn",
		fmt.Sprintf(":wave: Hi! I'm an AI assistant you can put to work right here in Slack — ask me a question and I'll spin up a sandboxed agent (`%s`) to answer. You just added me to <#%s> and I'm ready now: by default I respond *only when @-mentioned* and keep my replies *in a thread*.",
			agent, channel), false, false), nil, nil)
	explain := slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn",
		"Want me to behave differently in *this* channel?\n\n"+
			"*1. When should I respond?*\n"+
			"   • *Watch all* — I read every message in the channel and respond when I can help.\n"+
			"   • *Only @mentions* — I stay quiet unless you `@`-mention me directly.\n\n"+
			"*2. Where should my replies go?*\n"+
			"   • *In channel* — my replies post in the channel, visible to everyone.\n"+
			"   • *In threads* — my replies stay in a thread under your message, keeping the channel tidy.\n\n"+
			"Pick a combination (you can change it later by removing and re-adding me):",
		false, false), nil, nil)
	mk := func(i int, text string, mention, thread bool) *slack.ButtonBlockElement {
		return slack.NewButtonBlockElement(fmt.Sprintf("onb%d", i), onboardValue(channel, ns, agent, mention, thread),
			slack.NewTextBlockObject("plain_text", text, true, false))
	}
	actions := slack.NewActionBlock("claw-onboard",
		mk(0, "Watch all · reply in channel", false, false),
		mk(1, "Watch all · reply in thread", false, true),
		mk(2, "Only @mentions · in channel", true, false),
		mk(3, "Only @mentions · in thread", true, true),
	)
	blocks := []slack.Block{intro, explain, actions}
	if offerAdmin {
		blocks = append(blocks,
			slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn",
				"*One more thing:* this install has no *upgrade admin* yet — the person I ask before installing a new kube-claw release. Want it to be you? (First to claim it wins; an operator can change it later with `claw settings set upgrade-admin`.)",
				false, false), nil, nil),
			slack.NewActionBlock("claw-admin-claim",
				slack.NewButtonBlockElement("adminclaim", adminClaimValue,
					slack.NewTextBlockObject("plain_text", "Make me the upgrade admin", false, false))))
	}
	_, _, err := n.api.PostMessageContext(ctx, target, slack.MsgOptionBlocks(blocks...))
	return err
}

// PostAccessRequest DMs a granter an on-demand access request with the agent's
// justification (why) and who it's for, plus Approve/Deny buttons — so the
// granter can make an informed call.
func (n *Notifier) PostAccessRequest(ctx context.Context, granter, secretName, agentName, requester, reason, reqID string) error {
	text := fmt.Sprintf(":lock: *Access request*\nAgent `%s` is requesting the secret *%s*", agentName, secretName)
	if requester != "" {
		text += fmt.Sprintf("\n• For: <@%s>", requester)
	}
	if reason != "" {
		text += fmt.Sprintf("\n• Why: %s", reason)
	} else {
		text += "\n• Why: (no reason given)"
	}
	text += "\n\nApprove to grant this agent access."
	section := slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", text, false, false), nil, nil)
	approve := slack.NewButtonBlockElement("approve", ActionValue("approve", reqID),
		slack.NewTextBlockObject("plain_text", "Approve", false, false)).WithStyle(slack.StylePrimary)
	deny := slack.NewButtonBlockElement("deny", ActionValue("deny", reqID),
		slack.NewTextBlockObject("plain_text", "Deny", false, false)).WithStyle(slack.StyleDanger)
	actions := slack.NewActionBlock("claw-access", approve, deny)
	_, _, err := n.api.PostMessageContext(ctx, granter, slack.MsgOptionBlocks(section, actions))
	return err
}

// PostUpgradePrompt DMs the upgrade admin about a new release (DESIGN.md §24.4).
// canApply=true adds Upgrade / Skip / Later buttons; false is the notify-only
// degradation (requiresHelmUpgrade, minSupervisorVersion, custom registry, or
// manual mode) and explains how to upgrade instead.
func (n *Notifier) PostUpgradePrompt(ctx context.Context, admin, current, available, notes, reason string, containsMigration, canApply bool) error {
	text := fmt.Sprintf(":arrow_up: *kube-claw %s is available* (you're running %s).", available, current)
	if notes != "" {
		text += "\n> " + notes
	}
	if containsMigration {
		text += "\n:warning: This release migrates the database — if it fails I'll hold for a human instead of auto-rolling-back."
	}
	section := slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", text, false, false), nil, nil)
	if !canApply {
		manual := slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn",
			fmt.Sprintf("This release can't be self-applied (%s) — upgrade with `./scripts/install.sh` when ready.", reason),
			false, false), nil, nil)
		_, _, err := n.api.PostMessageContext(ctx, admin, slack.MsgOptionBlocks(section, manual))
		return err
	}
	mk := func(action, label string) *slack.ButtonBlockElement {
		return slack.NewButtonBlockElement("upg-"+action, UpgradeActionValue(action, available),
			slack.NewTextBlockObject("plain_text", label, false, false))
	}
	actions := slack.NewActionBlock("claw-upgrade",
		mk("approve", "Upgrade").WithStyle(slack.StylePrimary),
		mk("skip", "Skip this version"),
		mk("later", "Remind me later"))
	_, _, err := n.api.PostMessageContext(ctx, admin, slack.MsgOptionBlocks(section, actions))
	return err
}

// AddReaction adds an emoji reaction to a message (e.g. 🤔 while the agent
// works). Needs the bot's reactions:write scope.
func (n *Notifier) AddReaction(ctx context.Context, channel, ts, name string) error {
	return n.api.AddReactionContext(ctx, name, slack.ItemRef{Channel: channel, Timestamp: ts})
}

// RemoveReaction removes a previously-added reaction (best-effort).
func (n *Notifier) RemoveReaction(ctx context.Context, channel, ts, name string) error {
	return n.api.RemoveReactionContext(ctx, name, slack.ItemRef{Channel: channel, Timestamp: ts})
}
