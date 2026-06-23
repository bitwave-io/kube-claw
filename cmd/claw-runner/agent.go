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
		"IMPORTANT: if you already requested a secret and the user now says they've added it / asks you to check, call `request_secret` AGAIN with the same name — that installs the value. Do NOT use bash to look for it; the value is only pulled into the container by request_secret.",
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
		Description: anthropic.String("Request AND retrieve a credential. If the secret already exists but you're not " +
			"granted access, this opens an access request that the secret's approvers must approve in Slack (your `reason` " +
			"is shown to them). If it doesn't exist, the user is DMed a one-time link to provide it. Once approved/provided, " +
			"call this AGAIN with the same name to install it — the value is written to a file, the env var is set, and the " +
			"path is returned. This tool is the ONLY way to get the value into the container; do NOT bash-check for it."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"name":        map[string]any{"type": "string", "description": "secret name (use the EXACT name if it's listed as available to you), e.g. gcp-billing-readonly"},
				"description": map[string]any{"type": "string", "description": "what the secret is and how you'll use it"},
				"reason":      map[string]any{"type": "string", "description": "WHY you need it for this task and WHO it's for — shown to the approver so they can make an informed call"},
				"env_var": map[string]any{"type": "string", "description": "OPTIONAL: the env var that should point at the credential file, " +
					"e.g. GOOGLE_APPLICATION_CREDENTIALS for a GCP service-account key, AWS_SHARED_CREDENTIALS_FILE for AWS. " +
					"Set GOOGLE_APPLICATION_CREDENTIALS for GCP keys and I'll also activate it for the gcloud CLI. Leave empty to just get the file path."},
			},
			Required: []string{"name", "description", "reason"},
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

// loadAvailableSecrets appends the secrets the agent can request/retrieve (names
// + descriptions, never values) to the system prompt, so it uses an existing key
// by name instead of asking the user for a new one.
func (s *agentSession) loadAvailableSecrets(ctx context.Context) {
	if s.controllerURL == "" || s.runID == "" || s.token == "" {
		return
	}
	url := fmt.Sprintf("%s/v1/runs/%s/available-secrets", s.controllerURL, s.runID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var out struct {
		Secrets []struct{ Name, Description string }
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || len(out.Secrets) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("\n\nCredentials already registered and available to you — call request_secret with the EXACT name to install and use one (if the value was already provided, it installs instantly with no user prompt):\n")
	for _, sc := range out.Secrets {
		fmt.Fprintf(&b, "- %s: %s\n", sc.Name, sc.Description)
	}
	s.sys += b.String()
}

// loadHistory seeds the session with the thread's prior turns from the controller
// so a cold-start pod (warm pod idled out) still remembers the conversation. A
// no-op for a brand-new thread. Called once at pod start, before the first turn.
func (s *agentSession) loadHistory(ctx context.Context, sessionID string) {
	if s.controllerURL == "" || sessionID == "" || s.token == "" {
		return
	}
	url := fmt.Sprintf("%s/v1/sessions/%s/history", s.controllerURL, sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var out struct {
		Turns []struct{ Input, Output string }
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return
	}
	for _, t := range out.Turns {
		s.messages = append(s.messages,
			anthropic.NewUserMessage(anthropic.NewTextBlock(t.Input)),
			anthropic.NewAssistantMessage(anthropic.NewTextBlock(t.Output)))
	}
	if len(out.Turns) > 0 {
		fmt.Printf("claw-runner: replayed %d prior turn(s) for session %s\n", len(out.Turns), sessionID)
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
					var in struct {
						Name        string `json:"name"`
						Description string `json:"description"`
						Reason      string `json:"reason"`
						EnvVar      string `json:"env_var"`
					}
					_ = json.Unmarshal(raw, &in)
					result = s.requestSecret(ctx, in.Name, in.Description, in.Reason, in.EnvVar)
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
func (s *agentSession) requestSecret(ctx context.Context, name, description, reason, envVar string) string {
	if s.controllerURL == "" || s.runID == "" || s.token == "" {
		return "request_secret is unavailable in this run (no controller binding)."
	}
	// Retrieve-first: if access is already granted + the value is present, install
	// it immediately. (Makes re-calling to "fetch" idempotent.)
	if path, content, ok := s.fetchRequested(ctx, name); ok {
		return s.install(ctx, name, path, content, envVar)
	}
	// Otherwise ask the controller: it provisions (DMs a link) or opens an access
	// request to the secret's approvers, depending on whether it exists/is granted.
	body, _ := json.Marshal(map[string]string{"name": name, "description": description, "reason": reason})
	if err := s.post(ctx, fmt.Sprintf("/v1/runs/%s/request-secret", s.runID), body); err != nil {
		return "Couldn't request the secret: " + err.Error()
	}
	// Poll briefly in case it's approved/provided right away.
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if path, content, ok := s.fetchRequested(ctx, name); ok {
			return s.install(ctx, name, path, content, envVar)
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Sprintf("Access to *%s* isn't granted yet — either an approver needs to approve the request, or the user needs to provide the value via the DM link. Once they have, call request_secret again with name=%q to install it (do NOT bash-check — only this tool installs the value).", name, name)
}

// install writes a fetched secret into the pod and wires it up for the request's
// tooling (generic env var; gcloud activation only for a GCP key).
func (s *agentSession) install(ctx context.Context, name, path string, content []byte, envVar string) string {
	if err := os.MkdirAll("/var/run/claw/secrets", 0o700); err != nil {
		return "Received the secret but couldn't prepare the secrets dir: " + err.Error()
	}
	if err := os.WriteFile(path, content, 0o400); err != nil {
		return "Received the secret but failed to write it to disk: " + err.Error()
	}
	extra := ""
	if envVar != "" {
		_ = os.Setenv(envVar, path)
		extra = fmt.Sprintf(" $%s points to it.", envVar)
	}
	if envVar == "GOOGLE_APPLICATION_CREDENTIALS" && haveCLI("gcloud") {
		actx, cancel := context.WithTimeout(ctx, 60*time.Second)
		out, aerr := exec.CommandContext(actx, "gcloud", "auth", "activate-service-account", "--key-file="+path).CombinedOutput()
		cancel()
		if aerr == nil {
			extra += " I've also activated it for the gcloud CLI."
		} else {
			extra += " (gcloud activate-service-account said: " + firstLine(out) + ")"
		}
	}
	return fmt.Sprintf("Got it — *%s* is now installed at %s.%s Retry your task now.", name, path, extra)
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
