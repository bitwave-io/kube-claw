package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// agentSession is one warm Slack thread: a Claude tool-use loop (claude-opus-4-8,
// adaptive thinking, bash + request_secret tools) whose message history persists
// across turns in memory — so follow-ups continue the same conversation.
type agentSession struct {
	client        anthropic.Client
	sys           string
	tools         []anthropic.ToolUnionParam
	messages      []anthropic.MessageParam
	controllerURL string
	token         string // CLAW_TOKEN, for on-demand secret requests
	runID         string
}

func newAgentSession(systemPrompt string) *agentSession {
	sys := strings.TrimSpace(systemPrompt)
	if sys == "" {
		sys = "You are a cloud operations assistant."
	}
	notes := []string{
		"You are running inside an isolated, ephemeral Linux container. You have a `bash` tool to run shell commands here.",
	}
	for _, cli := range []string{"gcloud", "aws", "az"} {
		if haveCLI(cli) {
			notes = append(notes, "The `"+cli+"` CLI is installed and already authenticated via mounted credentials.")
		}
	}
	for _, d := range manifestDescriptions() {
		if d != "" {
			notes = append(notes, "Available secret: "+d)
		}
	}
	notes = append(notes,
		"If a task needs a credential you don't have (a cloud key, an API token), DON'T give up — call the `request_secret` tool. It DMs the user a secure link to provide it, then writes it to a file in this container so you can use it.",
		"Prefer read-only commands. Answer the user's question concisely, then stop. This is a chat thread — you may be asked follow-up questions.")
	sys = sys + "\n\n" + strings.Join(notes, "\n")

	bashTool := anthropic.ToolParam{
		Name:        "bash",
		Description: anthropic.String("Run a shell command in the container; returns combined stdout+stderr. Use read-only commands."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"command": map[string]any{"type": "string", "description": "The shell command to run"},
			},
			Required: []string{"command"},
		},
	}
	reqSecretTool := anthropic.ToolParam{
		Name: "request_secret",
		Description: anthropic.String("Request a credential you need but don't have (e.g. a cloud service-account key). " +
			"This DMs the user a one-time secure link to provide it; once they do, it's written to a file in this " +
			"container and that path is returned (and $GOOGLE_APPLICATION_CREDENTIALS is set for GCP keys). " +
			"Call this when a task needs credentials that aren't already present, then retry."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"name":        map[string]any{"type": "string", "description": "short secret name, e.g. gcp-billing-readonly"},
				"description": map[string]any{"type": "string", "description": "what the secret is and how you'll use it"},
			},
			Required: []string{"name", "description"},
		},
	}
	return &agentSession{
		client:        anthropic.NewClient(), // reads ANTHROPIC_API_KEY
		sys:           sys,
		tools:         []anthropic.ToolUnionParam{{OfTool: &bashTool}, {OfTool: &reqSecretTool}},
		controllerURL: os.Getenv("CLAW_CONTROLLER_URL"),
		token:         os.Getenv("CLAW_TOKEN"),
		runID:         os.Getenv("CLAW_RUN_ID"),
	}
}

// turn runs one user message to a final answer, executing bash tool calls along
// the way. History accumulates on the session for the next turn.
func (s *agentSession) turn(ctx context.Context, userText string) (string, error) {
	s.messages = append(s.messages, anthropic.NewUserMessage(anthropic.NewTextBlock(userText)))
	adaptive := anthropic.ThinkingConfigAdaptiveParam{}

	var final []string
	for i := 0; i < 12; i++ { // bound the agentic loop per turn
		resp, err := s.client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.ModelClaudeOpus4_8,
			MaxTokens: 4096,
			System:    []anthropic.TextBlockParam{{Text: s.sys}},
			Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive},
			Tools:     s.tools,
			Messages:  s.messages,
		})
		if err != nil {
			return "", err
		}
		s.messages = append(s.messages, resp.ToParam())

		var turn []string
		var toolResults []anthropic.ContentBlockParamUnion
		for _, block := range resp.Content {
			switch v := block.AsAny().(type) {
			case anthropic.TextBlock:
				if t := strings.TrimSpace(v.Text); t != "" {
					turn = append(turn, t)
				}
			case anthropic.ToolUseBlock:
				raw := []byte(v.JSON.Input.Raw())
				var result string
				switch v.Name {
				case "request_secret":
					var in struct{ Name, Description string }
					_ = json.Unmarshal(raw, &in)
					result = s.requestSecret(ctx, in.Name, in.Description)
				default: // bash
					var in struct {
						Command string `json:"command"`
					}
					_ = json.Unmarshal(raw, &in)
					result = runBash(ctx, in.Command)
				}
				toolResults = append(toolResults, anthropic.NewToolResultBlock(block.ID, result, false))
			}
		}
		if len(turn) > 0 {
			final = turn
		}
		if resp.StopReason != anthropic.StopReasonToolUse {
			break
		}
		s.messages = append(s.messages, anthropic.NewUserMessage(toolResults...))
	}
	answer := strings.Join(final, "\n\n")
	if answer == "" {
		return "", fmt.Errorf("agent produced no text answer")
	}
	return answer, nil
}

// requestSecret asks the controller to collect a credential on demand: it DMs
// the user an intake link, then polls until the value is provided, writes it to
// the tmpfs secrets dir, and points $GOOGLE_APPLICATION_CREDENTIALS at it.
func (s *agentSession) requestSecret(ctx context.Context, name, description string) string {
	if s.controllerURL == "" || s.runID == "" || s.token == "" {
		return "request_secret is unavailable in this run (no controller binding)."
	}
	// 1. ask the controller to create the secret + DM the user a link.
	body, _ := json.Marshal(map[string]string{"name": name, "description": description})
	if err := s.post(ctx, fmt.Sprintf("/v1/runs/%s/request-secret", s.runID), body); err != nil {
		return "Couldn't request the secret: " + err.Error()
	}
	// 2. poll until the user provides the value (or we time out).
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		path, content, ok := s.fetchRequested(ctx, name)
		if ok {
			if err := os.MkdirAll("/var/run/claw/secrets", 0o700); err == nil {
				if werr := os.WriteFile(path, content, 0o400); werr == nil {
					// bq + client libs read GOOGLE_APPLICATION_CREDENTIALS; the gcloud
					// CLI does not, so also activate the service account for gcloud.
					_ = os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
					extra := ""
					if haveCLI("gcloud") {
						actx, cancel := context.WithTimeout(ctx, 60*time.Second)
						out, aerr := exec.CommandContext(actx, "gcloud", "auth", "activate-service-account", "--key-file="+path).CombinedOutput()
						cancel()
						if aerr == nil {
							extra = " I've activated it for gcloud, and $GOOGLE_APPLICATION_CREDENTIALS is set for bq."
						} else {
							extra = " (note: gcloud activate-service-account said: " + firstLine(out) + ")"
						}
					}
					return fmt.Sprintf("Got it — *%s* is now available at %s.%s Retry your task now.", name, path, extra)
				}
			}
			return "Received the secret but failed to write it to disk."
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Sprintf("I've DM'd the user a one-time link to add *%s*. They haven't provided it yet — tell them to check their DMs, then ask me again.", name)
}

func (s *agentSession) post(ctx context.Context, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.controllerURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("controller returned %s", resp.Status)
	}
	return nil
}

// fetchRequested returns (path, decoded value, true) once the secret is provided.
func (s *agentSession) fetchRequested(ctx context.Context, name string) (string, []byte, bool) {
	url := fmt.Sprintf("%s/v1/runs/%s/requested-secret?name=%s", s.controllerURL, s.runID, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil, false
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, false // 204 = not provided yet
	}
	var out struct{ Path, Content string }
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return "", nil, false
	}
	val, err := base64.StdEncoding.DecodeString(out.Content)
	if err != nil {
		return "", nil, false
	}
	return out.Path, val, true
}

// runBash executes one shell command in the writable /workspace, with a timeout
// and a bounded output. The container's read-only rootfs + dropped caps + tmpfs
// secrets are the sandbox; this is not a second security boundary.
func runBash(parent context.Context, cmd string) string {
	if strings.TrimSpace(cmd) == "" {
		return "(empty command)"
	}
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "bash", "-lc", cmd)
	c.Dir = "/workspace"
	out, _ := c.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if len(s) > 8000 {
		s = s[:8000] + "\n…(truncated)"
	}
	if s == "" {
		s = "(no output)"
	}
	return s
}
