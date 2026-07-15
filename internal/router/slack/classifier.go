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
	if strings.TrimSpace(message) == "" {
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
		"unrequested commentary is disruptive, not helpful. When in doubt, reply NO. Output ONLY YES or NO."
	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     "claude-haiku-4-5",
		MaxTokens: 4,
		System:    []anthropic.TextBlockParam{{Text: sys}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Message:\n" + message))},
	})
	if err != nil {
		return false
	}
	var out string
	for _, b := range resp.Content {
		if t, ok := b.AsAny().(anthropic.TextBlock); ok {
			out += t.Text
		}
	}
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(out)), "YES")
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
		"(name: what it's for), reply with ONLY the single best agent name from the list. Output just the name."
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
	pick = strings.ToLower(strings.TrimSpace(pick))
	for _, a := range agents {
		if strings.ToLower(a.Name) == pick {
			return a.Namespace, a.Name
		}
	}
	return "", ""
}
