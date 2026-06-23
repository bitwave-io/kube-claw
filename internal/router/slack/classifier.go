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
