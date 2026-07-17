package slack

import (
	"context"
	"html"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Runnable adapts the Router to a controller-runtime Runnable (off unless tokens
// are configured). Leader-only so a single connection handles events.
type Runnable struct {
	Router   *Router
	AppToken string
	BotToken string
}

func (r *Runnable) NeedLeaderElection() bool { return true }

func (r *Runnable) Start(ctx context.Context) error {
	return r.Router.Start(ctx, r.AppToken, r.BotToken)
}

// Start connects to Slack over Socket Mode and dispatches events to the router.
// It is only invoked when app + bot tokens are configured (off by default).
// This transport requires real Slack credentials to exercise; the routing and
// approval logic it calls is unit-tested separately.
func (r *Router) Start(ctx context.Context, appToken, botToken string) error {
	lg := logf.Log.WithName("slack")
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	if at, err := api.AuthTestContext(ctx); err == nil {
		r.BotUserID = at.UserID
		lg.Info("slack identity", "botUser", at.UserID)
	}
	sm := socketmode.New(api)

	go func() {
		for evt := range sm.Events {
			switch evt.Type {
			case socketmode.EventTypeEventsAPI:
				if evt.Request != nil {
					sm.Ack(*evt.Request)
				}
				r.onEvent(ctx, evt)
			case socketmode.EventTypeInteractive:
				if evt.Request != nil {
					sm.Ack(*evt.Request)
				}
				r.onInteractive(ctx, api, evt)
			}
		}
	}()
	lg.Info("connecting to Slack (socket mode)")
	return sm.RunContext(ctx)
}

func (r *Router) onEvent(ctx context.Context, evt socketmode.Event) {
	lg := logf.Log.WithName("slack")
	api, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		return
	}
	if api.Type != slackevents.CallbackEvent {
		return
	}
	switch e := api.InnerEvent.Data.(type) {
	case *slackevents.MemberJoinedChannelEvent:
		// The bot itself was added to a channel → ask the inviter how to behave.
		if e.User != r.BotUserID || r.Notifier == nil {
			return
		}
		target := e.Inviter
		if target == "" {
			target = e.Channel // no inviter (e.g. created with the app) → ask in-channel
		}
		if err := r.Notifier.PostOnboarding(ctx, target, e.Channel, r.agentsNS(), r.DefaultAgent); err != nil {
			lg.Error(err, "post onboarding")
		} else {
			lg.Info("posted onboarding prompt", "channel", e.Channel, "inviter", e.Inviter)
		}
	case *slackevents.AppMentionEvent:
		runID, err := r.HandleMessage(ctx, e.TimeStamp, e.Channel, threadOr(e.ThreadTimeStamp, e.TimeStamp), r.agentText(e.User, e.Text), true, e.User)
		if err != nil {
			lg.Error(err, "handle app_mention")
		} else if runID != "" {
			lg.Info("created run from mention", "run", runID, "channel", e.Channel)
			r.react(ctx, e.Channel, e.TimeStamp)
		}
	case *slackevents.MessageEvent:
		if e.BotID != "" {
			return // ignore bot messages (incl. our own)
		}
		// A DM to the bot is a command (e.g. "register secret ..."), not a run.
		if e.ChannelType == "im" {
			reply := r.HandleDM(ctx, e.User, e.Text)
			if reply != "" && r.Notifier != nil {
				if perr := r.Notifier.PostReply(ctx, e.Channel, "", reply); perr != nil {
					lg.Error(perr, "post DM reply")
				}
			}
			return
		}
		// Detect an @mention of the bot before stripping it out (a mention arrives
		// as both app_mention and message events; SeenEvent dedupes by timestamp,
		// so whichever path runs first must treat it as an explicit request).
		mentioned := r.BotUserID != "" && strings.Contains(e.Text, "<@"+r.BotUserID+">")
		// A reply in a thread the bot started continues that conversation (no
		// @mention needed); otherwise fall back to route matching.
		if e.ThreadTimeStamp != "" && e.ThreadTimeStamp != e.TimeStamp {
			if runID, err := r.HandleThreadReply(ctx, e.TimeStamp, e.Channel, e.ThreadTimeStamp, r.agentText(e.User, e.Text), mentioned, e.User); err != nil {
				lg.Error(err, "handle thread reply")
			} else if runID != "" {
				lg.Info("created follow-up run from thread reply", "run", runID, "channel", e.Channel)
				r.react(ctx, e.Channel, e.TimeStamp)
				return
			}
		}
		runID, err := r.HandleMessage(ctx, e.TimeStamp, e.Channel, threadOr(e.ThreadTimeStamp, e.TimeStamp), r.agentText(e.User, e.Text), mentioned, e.User)
		if err != nil {
			lg.Error(err, "handle message")
		} else if runID != "" {
			lg.Info("created run from message", "run", runID, "channel", e.Channel)
			r.react(ctx, e.Channel, e.TimeStamp)
		}
	}
}

// agentText turns a raw Slack message into the text the agent sees: the bot's
// own @mention is stripped (it's addressing, not content), Slack's &amp;/&lt;/&gt;
// escapes are undone, and the sender's id is prefixed (<@U…>: …) so the agent
// can tell speakers apart in multi-user threads.
func (r *Router) agentText(user, text string) string {
	if r.BotUserID != "" {
		text = strings.ReplaceAll(text, "<@"+r.BotUserID+">", "")
	}
	text = strings.TrimSpace(html.UnescapeString(text))
	if user == "" || text == "" {
		return text
	}
	return "<@" + user + ">: " + text
}

// react adds a 👀 to the triggering message the moment we start a run, so the
// user sees the message was picked up and a container is spinning up.
func (r *Router) react(ctx context.Context, channel, ts string) {
	if r.Notifier == nil {
		return
	}
	if err := r.Notifier.AddReaction(ctx, channel, ts, "eyes"); err != nil {
		logf.Log.WithName("slack").Error(err, "add eyes reaction")
	}
}

func (r *Router) onInteractive(ctx context.Context, api *slack.Client, evt socketmode.Event) {
	cb, ok := evt.Data.(slack.InteractionCallback)
	if !ok {
		return
	}
	for _, a := range cb.ActionCallback.BlockActions {
		var msg string
		if strings.HasPrefix(a.Value, "onboard|") {
			msg = r.HandleOnboard(ctx, a.Value)
		} else {
			msg = r.HandleApproval(ctx, a.Value, cb.User.ID)
		}
		_, _, _ = api.PostMessage(cb.Channel.ID, slack.MsgOptionText(msg, false))
	}
}

func threadOr(thread, ts string) string {
	if thread != "" {
		return thread
	}
	return ts
}
