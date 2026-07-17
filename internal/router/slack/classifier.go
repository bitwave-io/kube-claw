package slack

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AgentChoice is one routable agent (name + a "what it does" description, used by
// the classifier to pick the best-fit agent for a request).
type AgentChoice struct {
	Namespace   string
	Name        string
	Description string
}

// Classifier picks the best-fit agent for a request by matching it against the
// agents' descriptions. It runs a fast/cheap Haiku call in the message path; on
// any error it returns "" (the caller falls back to the channel's default agent).
type Classifier struct {
	client anthropic.Client
}

func NewClassifier(apiKey string) *Classifier {
	return &Classifier{client: anthropic.NewClient(option.WithAPIKey(apiKey))}
}

// ShouldRespond gates UNPROMPTED replies in an "active participant" channel
// (mentionRequired=false): it returns true only when the bot is highly confident
// it has something genuinely useful to add. The bar is deliberately high — most
// chatter (greetings, banter, coordination between people, vague statements) must
// return false so the bot stays quiet unless it can clearly help. A cheap Haiku
// call; on any error it returns false (fail closed → stay silent, never spam).
func (c *Classifier) ShouldRespond(ctx context.Context, message string) bool {
	msg := strings.TrimSpace(message)
	// Too short to be an actionable request ("lol", "ok", "why?") — don't spend
	// an LLM call deciding; an unprompted bot reply to these is always noise.
	if len(msg) < 6 {
		return false
	}
	sys := "You decide whether an AI cloud-operations assistant should speak up UNPROMPTED in a team " +
		"chat channel — often a busy incident channel — where it was NOT directly addressed. Reply YES only " +
		"if you are about 90% sure the assistant has something concrete and clearly useful to add RIGHT NOW — " +
		"e.g. a question asked to the room at large that it can answer, a problem it can help diagnose, or a " +
		"request for cloud/ops help that nobody has been asked to handle. " +
		"Reply NO whenever the message has a specific human addressee — an @mention, a name (\"Sarah, can you " +
		"check the LB?\"), or a reply/answer to a specific person. If it is someone's turn to speak and that " +
		"someone is not the assistant, reply NO even if the assistant could technically help. " +
		"Also reply NO for people coordinating with each other (acks, handoffs, status updates, \"I'm on it\", " +
		"\"looking\"), greetings, banter, opinions, and vague or ambiguous statements — during an incident, " +
		"unrequested commentary is disruptive, not helpful. When in doubt, reply NO. Output ONLY YES or NO.\n\n" +
		"Examples:\n" +
		"\"why is our GKE bill suddenly 3x higher this month?\" → YES\n" +
		"\"anyone know how to list buckets that are public?\" → YES\n" +
		"\"morning all 👋\" → NO\n" +
		"\"<@U02ALICE>: <@U0DAVE> can you look at the deploy when you're back?\" → NO\n" +
		"\"ugh, cloud costs\" → NO"
	return c.yesNo(ctx, sys, "Message:\n"+msg, false)
}

// ShouldRespondInThread gates a reply inside a thread the bot is already
// participating in. Unlike ShouldRespond, the default here is OPEN — a reply in
// the bot's own thread is usually addressed to it — so it returns false only
// when the message is clearly meant for someone else. It also fails open on
// error: dropping a legitimate follow-up is worse than one extra reply.
func (c *Classifier) ShouldRespondInThread(ctx context.Context, message string) bool {
	if strings.TrimSpace(message) == "" {
		return false
	}
	sys := "An AI cloud-operations assistant is active in a Slack thread it was brought into. You decide " +
		"whether the latest reply in that thread is meant for the assistant. Reply YES unless the message is " +
		"CLEARLY not addressed to it — e.g. it @mentions or names another person as the one being asked, or " +
		"it is plainly a side-conversation between two humans. Follow-up questions, corrections, " +
		"acknowledgements, and anything ambiguous ARE for the assistant: reply YES. Output ONLY YES or NO."
	return c.yesNo(ctx, sys, "Thread reply:\n"+message, true)
}

// yesNo runs a tiny Haiku YES/NO classification; onErr is returned when the
// call fails (false = fail closed / stay silent, true = fail open / respond).
func (c *Classifier) yesNo(ctx context.Context, sys, user string, onErr bool) bool {
	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     "claude-haiku-4-5",
		MaxTokens: 4,
		System:    []anthropic.TextBlockParam{{Text: sys}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
	})
	if err != nil {
		return onErr
	}
	var out string
	for _, b := range resp.Content {
		if t, ok := b.AsAny().(anthropic.TextBlock); ok {
			out += t.Text
		}
	}
	verdict := strings.ToUpper(strings.TrimSpace(out))
	if onErr {
		return !strings.HasPrefix(verdict, "NO") // default open
	}
	return strings.HasPrefix(verdict, "YES") // default closed
}

// PickAgent returns the namespace+name of the best-fit agent, or ("","") to fall
// back. With a single agent it returns it directly (no LLM call).
func (c *Classifier) PickAgent(ctx context.Context, request string, agents []AgentChoice) (ns, name string) {
	if len(agents) == 0 || strings.TrimSpace(request) == "" {
		return "", ""
	}
	if len(agents) == 1 {
		return agents[0].Namespace, agents[0].Name
	}
	var list strings.Builder
	for _, a := range agents {
		fmt.Fprintf(&list, "- %s: %s\n", a.Name, a.Description)
	}
	sys := "You route a user request to the best-fit agent. Given the request and a list of agents " +
		"(name: what it's for), reply with ONLY the single best agent name from the list. " +
		"If none of the agents is a reasonable fit for the request, reply NONE. Output just the name, nothing else."
	user := fmt.Sprintf("Request:\n%s\n\nAgents:\n%s", request, list.String())

	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     "claude-haiku-4-5",
		MaxTokens: 16,
		System:    []anthropic.TextBlockParam{{Text: sys}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
	})
	if err != nil {
		return "", ""
	}
	var pick string
	for _, b := range resp.Content {
		if t, ok := b.AsAny().(anthropic.TextBlock); ok {
			pick += t.Text
		}
	}
	// Normalize: models love to wrap the name in backticks/quotes or add a period;
	// without this the exact-match below silently falls back on a correct pick.
	pick = strings.ToLower(strings.TrimSpace(pick))
	if f := strings.Fields(pick); len(f) > 0 {
		pick = f[0]
	}
	pick = strings.Trim(pick, "`'\".,:!")
	if pick == "" || pick == "none" {
		return "", "" // no clear fit — fall back to the channel's agent
	}
	for _, a := range agents {
		if strings.ToLower(a.Name) == pick {
			return a.Namespace, a.Name
		}
	}
	return "", ""
}
