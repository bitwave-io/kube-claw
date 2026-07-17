package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// agentSession is one warm Slack thread: a Claude tool-use loop (claude-opus-4-8,
// adaptive thinking, bash + request_secret tools) whose message history persists
// across turns in memory — so follow-ups continue the same conversation.
type agentSession struct {
	client        anthropic.Client
	model         anthropic.Model
	sys           string
	tools         []anthropic.ToolUnionParam
	messages      []anthropic.MessageParam
	controllerURL string
	token         string // CLAW_TOKEN, for on-demand secret requests
	runID         string

	mu   sync.Mutex // guards step
	step string     // the agent's current narrated step, surfaced by the heartbeat
}

func (s *agentSession) setStep(t string) { s.mu.Lock(); s.step = t; s.mu.Unlock() }
func (s *agentSession) getStep() string  { s.mu.Lock(); defer s.mu.Unlock(); return s.step }

func newAgentSession(systemPrompt string) *agentSession {
	sys := strings.TrimSpace(systemPrompt)
	if sys == "" {
		sys = "You are a cloud operations assistant."
	}
	notes := []string{
		"You are running inside an isolated, ephemeral Linux container. You have a `bash` tool to run shell commands here.",
		"Today's date is " + time.Now().UTC().Format("Monday, January 2, 2006") + " (UTC).",
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
		"Bash commands are killed after 90 seconds and long output is truncated — use non-interactive flags, and narrow output with grep/head/--format instead of dumping everything.",
		"Before each slow or significant command, say in one short sentence what you're about to do. That narration is shown to the user as live progress while they wait.",
		"You are chatting in Slack. Format replies as Slack mrkdwn: *single asterisks* for bold, _underscores_ for italic, `backticks` for code, ``` fenced blocks ``` for command output, and simple - bullets. NEVER use Markdown headers (#), **double asterisks**, or tables — they render as literal characters in Slack. Messages may arrive prefixed with the sender's Slack id (`<@U…>:`), and you can mention a user the same way.",
		"If a task needs a credential you don't have (a cloud key, an API token), DON'T give up — call the `request_secret` tool. It DMs the user a secure link to provide it, then writes it to a file in this container so you can use it.",
		"IMPORTANT: if you already requested a secret and the user now says they've added it / asks you to check, call `request_secret` AGAIN with the same name — that installs the value. Do NOT use bash to look for it; the value is only pulled into the container by request_secret.",
		"Prefer read-only commands. Answer the user's question concisely, then stop. This is a chat thread — you may be asked follow-up questions.")
	sys = sys + "\n\n" + strings.Join(notes, "\n")

	model := anthropic.ModelClaudeOpus4_8
	if m := os.Getenv("CLAW_MODEL"); m != "" {
		model = anthropic.Model(m)
	}

	bashTool := anthropic.ToolParam{
		Name:        "bash",
		Description: anthropic.String("Run a shell command in the container; returns combined stdout+stderr. Commands are killed after 90s and output beyond ~8KB is truncated in the middle — narrow output with grep/head/--format. Prefer read-only commands; never run interactive ones."),
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
		model:         model,
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
	s.compactHistory()
	s.messages = append(s.messages, anthropic.NewUserMessage(anthropic.NewTextBlock(userText)))
	adaptive := anthropic.ThinkingConfigAdaptiveParam{}

	// Heartbeat: for turns that run long, post in-thread progress (~every 60s)
	// describing the agent's current step, so a slow operation isn't silent.
	s.setStep("")
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go s.heartbeat(hbCtx)

	var final []string
	for { // the turn's ctx deadline (see main.go) bounds the agentic loop
		markCacheBreakpoint(s.messages)
		resp, err := s.callModel(ctx, anthropic.MessageNewParams{
			Model:     s.model,
			MaxTokens: 32000,
			System: []anthropic.TextBlockParam{
				// Breakpoint here caches the tools + system prefix across calls.
				{Text: s.sys, CacheControl: anthropic.NewCacheControlEphemeralParam()},
			},
			Thinking: anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive},
			Tools:    s.tools,
			Messages: s.messages,
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
					s.setStep("Requesting credential " + in.Name + "…")
					result = s.requestSecret(ctx, in.Name, in.Description, in.Reason, in.EnvVar)
				default: // bash
					var in struct {
						Command string `json:"command"`
					}
					_ = json.Unmarshal(raw, &in)
					// The running command is what the heartbeat reports — it's always
					// current, unlike narration text (which may be a conclusion).
					s.setStep("Running `" + firstLine([]byte(in.Command)) + "`…")
					result = runBash(ctx, in.Command)
				}
				toolResults = append(toolResults, anthropic.NewToolResultBlock(block.ID, result, false))
			}
		}
		if len(turn) > 0 {
			final = turn
			// Between tool calls the newest narration is the best progress signal.
			if len(toolResults) > 0 {
				s.setStep(turn[len(turn)-1])
			}
		}
		if resp.StopReason == anthropic.StopReasonToolUse {
			s.messages = append(s.messages, anthropic.NewUserMessage(toolResults...))
			continue
		}
		if resp.StopReason == anthropic.StopReasonMaxTokens {
			// Cut off by the output limit — don't present a truncated reply as the
			// answer. Completed tool calls (if any) still ran; feed their results
			// back, otherwise nudge the model to pick up where it stopped.
			if len(toolResults) > 0 {
				s.messages = append(s.messages, anthropic.NewUserMessage(toolResults...))
			} else {
				s.messages = append(s.messages, anthropic.NewUserMessage(anthropic.NewTextBlock(
					"(Your reply was cut off by the output limit — continue where you left off, and keep it brief.)")))
			}
			continue
		}
		break
	}
	answer := strings.Join(final, "\n\n")
	if answer == "" {
		return "", fmt.Errorf("agent produced no text answer")
	}
	return answer, nil
}

// markCacheBreakpoint keeps exactly one ephemeral cache breakpoint on the newest
// cacheable content block, clearing older ones, so each model call reuses the
// previous call's cached prefix instead of re-reading the whole conversation.
// (The system prompt carries its own breakpoint; the API allows four total.)
func markCacheBreakpoint(messages []anthropic.MessageParam) {
	newest := true
	for i := len(messages) - 1; i >= 0; i-- {
		for j := len(messages[i].Content) - 1; j >= 0; j-- {
			cc := messages[i].Content[j].GetCacheControl()
			if cc == nil {
				continue // block type without cache_control (e.g. thinking)
			}
			if newest {
				*cc = anthropic.NewCacheControlEphemeralParam()
				newest = false
			} else {
				*cc = anthropic.CacheControlEphemeralParam{}
			}
		}
	}
}

// compactHistory trims bulky tool results in older turns so a long-lived warm
// session doesn't grow without bound. The recent tail stays intact; old tool
// output is rarely needed verbatim and each turn's text answer survives in full.
func (s *agentSession) compactHistory() {
	const keepRecent = 16     // messages left untouched at the tail
	const keepPerResult = 500 // bytes retained of each older tool result
	if len(s.messages) <= keepRecent {
		return
	}
	for i := range s.messages[:len(s.messages)-keepRecent] {
		for _, block := range s.messages[i].Content {
			tr := block.OfToolResult
			if tr == nil {
				continue
			}
			for k := range tr.Content {
				if txt := tr.Content[k].OfText; txt != nil && len(txt.Text) > keepPerResult {
					txt.Text = txt.Text[:keepPerResult] + "\n…(older tool output trimmed)"
				}
			}
		}
	}
}

// callModel calls Claude with visible retry-with-backoff: on a (usually transient,
// e.g. network/DNS) failure it posts "…retrying in Xs…" to the thread and waits,
// up to a few attempts, before giving up so the caller can report a clean failure.
func (s *agentSession) callModel(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	backoffs := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second}
	var lastErr error
	for attempt := 0; ; attempt++ {
		resp, err := s.client.Messages.New(ctx, params)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		fmt.Fprintf(os.Stderr, "claw-runner: model call attempt %d failed: %v\n", attempt+1, err)
		// Only transient failures are worth retrying. A 400/401/403 is permanent —
		// retrying spams the thread with "retrying…" for a request that can't succeed.
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) && !retryableStatus(apiErr.StatusCode) {
			return nil, lastErr
		}
		if attempt >= len(backoffs) {
			return nil, lastErr
		}
		wait := backoffs[attempt]
		s.postProgress(ctx, fmt.Sprintf("⚠️ That request to the model failed (%s) — retrying in %ds… (attempt %d of %d)",
			shortErr(err), int(wait.Seconds()), attempt+2, len(backoffs)+1))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// retryableStatus reports whether an API error status is worth retrying
// (timeouts, rate limits, server-side errors — not client errors like 400/401).
func retryableStatus(code int) bool {
	switch code {
	case 408, 429, 500, 502, 503, 504, 529:
		return true
	}
	return false
}

// shortErr maps a raw error to a short, human phrase for status messages.
func shortErr(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "no such host"), strings.Contains(s, "dial tcp"), strings.Contains(s, "lookup "):
		return "network/DNS issue"
	case strings.Contains(s, "deadline"), strings.Contains(s, "timeout"):
		return "timeout"
	case strings.Contains(s, "connection refused"), strings.Contains(s, "connection reset"):
		return "connection dropped"
	case strings.Contains(s, "429"), strings.Contains(s, "rate"), strings.Contains(s, "overloaded"), strings.Contains(s, "529"):
		return "rate-limited/overloaded"
	default:
		return "temporary error"
	}
}

// heartbeat posts an in-thread progress update roughly every 60s while a turn
// runs long, so slow operations report what they're doing instead of going
// silent. The first post is at ~60s, so quick turns stay clean.
func (s *agentSession) heartbeat(ctx context.Context) {
	if s.controllerURL == "" || s.runID == "" {
		return
	}
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			msg := "⏳ Still working on it…"
			if step := s.getStep(); step != "" {
				if len(step) > 280 {
					step = step[:280] + "…"
				}
				msg = "⏳ " + step
			}
			s.postProgress(ctx, msg)
		}
	}
}

// postProgress sends an intermediate, in-thread status message (best-effort).
func (s *agentSession) postProgress(ctx context.Context, text string) {
	body, _ := json.Marshal(map[string]string{"text": text})
	url := fmt.Sprintf("%s/v1/runs/%s/progress", s.controllerURL, s.runID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	if resp, e := http.DefaultClient.Do(req); e == nil {
		resp.Body.Close()
	}
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
		select {
		case <-ctx.Done():
			return fmt.Sprintf("The wait for *%s* was interrupted — call request_secret again with name=%q to retry.", name, name)
		case <-time.After(5 * time.Second):
		}
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
	out, err := c.CombinedOutput()
	s := strings.TrimSpace(string(out))
	// Truncate in the middle, keeping the tail — for a failed cloud CLI command
	// the error is usually at the end, and a head-only cut hides it.
	const headKeep, tailKeep = 6000, 2000
	if len(s) > headKeep+tailKeep+200 {
		s = s[:headKeep] +
			fmt.Sprintf("\n…(%d bytes omitted — narrow the output with grep/head/--format)…\n", len(s)-headKeep-tailKeep) +
			s[len(s)-tailKeep:]
	}
	// Distinguish a command that genuinely printed nothing from one that never
	// ran. If bash itself can't be started (e.g. missing from a minimal image),
	// CombinedOutput returns an exec error with no output — surface that instead
	// of a bare "(no output)", which reads like a wedged shell and sends the
	// agent down a dead end.
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// The command ran and exited non-zero. Its stderr is in out; include
			// the exit status so the agent can react to failures.
			status := fmt.Sprintf("(exit status %d)", ee.ExitCode())
			if s == "" {
				return status
			}
			return s + "\n" + status
		}
		// bash never launched (not found, timeout, etc.) — a real tooling problem.
		return fmt.Sprintf("(command could not be run: %v)", err)
	}
	if s == "" {
		s = "(no output)"
	}
	return s
}
