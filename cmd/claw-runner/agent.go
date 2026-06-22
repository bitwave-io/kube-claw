package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// runAgent runs a Claude tool-use loop (DESIGN.md §agent-loop). It gives the
// model a single `bash` tool that executes in this isolated, ephemeral container
// — where the cloud CLIs (gcloud/aws/az) and the materialized secrets already
// live — and loops until the model produces a final answer. The model is the
// latest Opus with adaptive thinking. ANTHROPIC_API_KEY is injected by the run
// engine from a platform secret; if absent, the caller falls back to the stub.
func runAgent(ctx context.Context, systemPrompt, input string) (string, error) {
	client := anthropic.NewClient() // reads ANTHROPIC_API_KEY from env

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
	notes = append(notes, "Prefer read-only commands. Answer the user's question concisely, then stop.")
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
	tools := []anthropic.ToolUnionParam{{OfTool: &bashTool}}
	messages := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(input))}
	adaptive := anthropic.ThinkingConfigAdaptiveParam{}

	var final []string
	for i := 0; i < 12; i++ { // bound the agentic loop
		resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.ModelClaudeOpus4_8,
			MaxTokens: 4096,
			System:    []anthropic.TextBlockParam{{Text: sys}},
			Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive},
			Tools:     tools,
			Messages:  messages,
		})
		if err != nil {
			return "", err
		}
		messages = append(messages, resp.ToParam())

		var turn []string
		var toolResults []anthropic.ContentBlockParamUnion
		for _, block := range resp.Content {
			switch v := block.AsAny().(type) {
			case anthropic.TextBlock:
				if t := strings.TrimSpace(v.Text); t != "" {
					turn = append(turn, t)
				}
			case anthropic.ToolUseBlock:
				var in struct {
					Command string `json:"command"`
				}
				_ = json.Unmarshal([]byte(v.JSON.Input.Raw()), &in)
				toolResults = append(toolResults, anthropic.NewToolResultBlock(block.ID, runBash(ctx, in.Command), false))
			}
		}
		if len(turn) > 0 {
			final = turn // keep the most recent turn's text as the answer
		}
		if resp.StopReason != anthropic.StopReasonToolUse {
			break
		}
		messages = append(messages, anthropic.NewUserMessage(toolResults...))
	}
	answer := strings.Join(final, "\n\n")
	if answer == "" {
		return "", fmt.Errorf("agent produced no text answer")
	}
	return answer, nil
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
