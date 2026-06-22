package slack

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/slack-go/slack"
)

// Notifier posts back to Slack: run replies (#2) and interactive PAM approval
// requests (#3). Constructed with the bot token; nil when Slack isn't configured.
// The live posting needs real Slack credentials to exercise.
type Notifier struct {
	api *slack.Client
}

func NewNotifier(botToken string) *Notifier {
	return &Notifier{api: slack.New(botToken)}
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
// HandleMessage as {"trigger":"slack","channel":"..."}). Empty if not Slack.
func SlackChannel(source string) string {
	var s struct {
		Trigger string `json:"trigger"`
		Channel string `json:"channel"`
	}
	if json.Unmarshal([]byte(source), &s) != nil || s.Trigger != "slack" {
		return ""
	}
	return s.Channel
}
