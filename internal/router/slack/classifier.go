package slack

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/traego/kube-claw/internal/store"
)

// Classifier picks the best base image for a request by matching it against the
// registry's "when to use" descriptions. It runs a fast/cheap Haiku call in the
// message path; on any error it returns "" (fall back to the agent's image).
type Classifier struct {
	client anthropic.Client
}

func NewClassifier(apiKey string) *Classifier {
	return &Classifier{client: anthropic.NewClient(option.WithAPIKey(apiKey))}
}

// PickImage returns the name of the best-fit base image, or "" for none/default.
func (c *Classifier) PickImage(ctx context.Context, request string, images []store.BaseImage) string {
	if len(images) == 0 || strings.TrimSpace(request) == "" {
		return ""
	}
	var list strings.Builder
	for _, im := range images {
		fmt.Fprintf(&list, "- %s: %s\n", im.Name, im.Description)
	}
	sys := "You route a user request to the best-fit tool image. Given the request and a list of " +
		"available images (name: when to use), reply with ONLY the single best image name from the " +
		"list — or the word none if no specialized tooling is needed. Output just the name, nothing else."
	user := fmt.Sprintf("Request:\n%s\n\nAvailable images:\n%snone: general request, no special tooling needed", request, list.String())

	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     "claude-haiku-4-5",
		MaxTokens: 16,
		System:    []anthropic.TextBlockParam{{Text: sys}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))},
	})
	if err != nil {
		return ""
	}
	var pick string
	for _, b := range resp.Content {
		if t, ok := b.AsAny().(anthropic.TextBlock); ok {
			pick += t.Text
		}
	}
	pick = strings.ToLower(strings.TrimSpace(pick))
	for _, im := range images {
		if strings.ToLower(im.Name) == pick {
			return im.Name
		}
	}
	return ""
}
