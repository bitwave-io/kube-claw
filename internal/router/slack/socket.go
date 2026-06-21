package slack

import (
	"context"

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
	case *slackevents.AppMentionEvent:
		runID, err := r.HandleMessage(ctx, e.TimeStamp, e.Channel, threadOr(e.ThreadTimeStamp, e.TimeStamp), e.Text, true)
		if err != nil {
			lg.Error(err, "handle app_mention")
		} else if runID != "" {
			lg.Info("created run from mention", "run", runID, "channel", e.Channel)
		}
	case *slackevents.MessageEvent:
		if e.BotID != "" {
			return // ignore bot messages (incl. our own)
		}
		runID, err := r.HandleMessage(ctx, e.TimeStamp, e.Channel, threadOr(e.ThreadTimeStamp, e.TimeStamp), e.Text, false)
		if err != nil {
			lg.Error(err, "handle message")
		} else if runID != "" {
			lg.Info("created run from message", "run", runID, "channel", e.Channel)
		}
	}
}

func (r *Router) onInteractive(ctx context.Context, api *slack.Client, evt socketmode.Event) {
	cb, ok := evt.Data.(slack.InteractionCallback)
	if !ok {
		return
	}
	for _, a := range cb.ActionCallback.BlockActions {
		msg := r.HandleApproval(ctx, a.Value, cb.User.ID)
		_, _, _ = api.PostMessage(cb.Channel.ID, slack.MsgOptionText(msg, false))
	}
}

func threadOr(thread, ts string) string {
	if thread != "" {
		return thread
	}
	return ts
}
