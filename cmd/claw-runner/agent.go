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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// noReply is the sentinel answer the agent returns when the latest message
// needs no response from it (e.g. two humans talking to each other). It flows
// through postOutput as a kind:"none" output — the run completes, the 👀
// marker clears, and nothing is posted to the thread.
const noReply = "NO_REPLY"

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
	runID         string
	agentName     string // CLAW_AGENT_NAME — the agent picked to service this thread

	// servedModel is the model ID the API reported serving the last call; the
	// first reply this pod posts is tagged with it and the agent name (a fresh
	// tag mid-thread means the warm pod restarted — useful when diagnosing
	// stalls/evictions).
	servedModel string
	modelTagged bool

	// claimTurn (set for warm Slack sessions) claims the next queued follow-up
	// run. While a turn runs, watchInterrupts is the only claimer: it lets the
	// turn absorb messages that arrive WHILE it runs instead of answering them
	// one-by-one afterwards with stale context, and it hard-cancels the turn on
	// a stop command.
	claimTurn func() (runID, text string, ok bool)

	mu   sync.Mutex // guards step + recent + the interrupt state below
	step string     // the agent's current narrated step, surfaced by the heartbeat
	// extraRuns holds run ids claimed mid-turn (the caller completes them — the
	// answer posts on the newest, older ones close silently); interrupts holds
	// their texts until the next model call injects them. aborted/stopText are
	// set by watchInterrupts when a user stop command cancels the turn.
	extraRuns  []string
	interrupts []string
	aborted    bool
	stopText   string // full text of the stop message (may carry a new instruction)
	// recent is a rolling log of the steps taken this turn (narration lines +
	// commands run), so the heartbeat can summarize what the agent has been
	// doing rather than only echoing the latest line. Reset at the start of a turn.
	recent []string
}

// setRunID switches the session's callback attribution to the given run. Each
// warm-loop turn serves a DIFFERENT run: progress updates, secret requests, and
// published artifacts must be stored and audited against the run that asked,
// not the pod's first run.
func (s *agentSession) setRunID(id string) { s.mu.Lock(); s.runID = id; s.mu.Unlock() }

func (s *agentSession) currentRunID() string { s.mu.Lock(); defer s.mu.Unlock(); return s.runID }

// setStep records the agent's current step and appends it to the turn's rolling
// activity log, so the heartbeat can show both what's happening now and a summary
// of what's already been done.
func (s *agentSession) setStep(t string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.step = t
	if t == "" {
		return
	}
	// Skip consecutive duplicates (e.g. re-narrating the same step).
	if n := len(s.recent); n > 0 && s.recent[n-1] == t {
		return
	}
	s.recent = append(s.recent, t)
	const maxRecent = 8 // keep the tail; the heartbeat shows the last few
	if len(s.recent) > maxRecent {
		s.recent = s.recent[len(s.recent)-maxRecent:]
	}
}

func (s *agentSession) getStep() string { s.mu.Lock(); defer s.mu.Unlock(); return s.step }

// resetActivity clears the rolling activity log at the start of a turn.
func (s *agentSession) resetActivity() { s.mu.Lock(); s.recent = nil; s.step = ""; s.mu.Unlock() }

// activitySummary returns the current step plus a short list of the recent steps
// taken this turn, for the heartbeat to report. done excludes the in-progress step.
func (s *agentSession) activitySummary() (current string, done []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current = s.step
	// The last entry in recent is usually the current step — omit it from "done".
	done = s.recent
	if n := len(done); n > 0 && done[n-1] == current {
		done = done[:n-1]
	}
	// Return a copy so callers can format without holding the lock.
	if len(done) > 4 {
		done = done[len(done)-4:] // show at most the last 4 completed steps
	}
	out := make([]string, len(done))
	copy(out, done)
	return current, out
}

// beginTurn resets the per-turn interrupt state.
func (s *agentSession) beginTurn() {
	s.mu.Lock()
	s.aborted, s.stopText, s.interrupts = false, "", nil
	s.mu.Unlock()
}

// takeInterrupts returns (and clears) the buffered mid-turn message texts.
func (s *agentSession) takeInterrupts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	texts := s.interrupts
	s.interrupts = nil
	return texts
}

// hasInterrupts reports whether a mid-turn message is waiting to be injected.
func (s *agentSession) hasInterrupts() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.interrupts) > 0
}

// takeExtraRuns returns (and clears) the runs claimed during the last turn.
func (s *agentSession) takeExtraRuns() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := s.extraRuns
	s.extraRuns = nil
	return ids
}

// isAborted reports whether the current turn was cancelled by a user stop.
func (s *agentSession) isAborted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aborted
}

// takeStopFollowup returns the stop message's text when it carried more than
// the bare stop word ("stop, check prod-a instead") — the caller runs it as a
// fresh turn once the old work is cancelled. Empty for a plain "stop".
func (s *agentSession) takeStopFollowup() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.stopText
	s.stopText = ""
	if t == "" {
		return ""
	}
	if _, rest := stopCommand(t); rest == "" {
		return ""
	}
	return t
}

// watchInterrupts claims queued follow-up runs every few seconds WHILE a turn
// runs (it is the turn's only claimer). Ordinary messages are buffered for
// injection at the next loop iteration; a stop command cancels the turn's
// context immediately — killing the in-flight model call, bash command, or
// approval wait — instead of waiting politely for the model to finish. Runs
// claimed here are completed by the caller when the turn ends.
func (s *agentSession) watchInterrupts(ctx context.Context, cancelTurn context.CancelFunc) {
	if s.claimTurn == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		for {
			if ctx.Err() != nil {
				return // turn ended — stop claiming so nothing is orphaned
			}
			id, text, ok := s.claimTurn()
			if !ok {
				break
			}
			stop, _ := stopCommand(text)
			s.mu.Lock()
			s.extraRuns = append(s.extraRuns, id)
			if stop {
				s.aborted, s.stopText = true, text
				s.mu.Unlock()
				cancelTurn()
				return
			}
			s.interrupts = append(s.interrupts, text)
			s.mu.Unlock()
		}
	}
}

// slackMention matches encoded Slack mentions (and the "<@U…>:" sender prefix)
// so stop detection sees the words, not the markup.
var slackMention = regexp.MustCompile(`<@[^>]+>:?`)

// stopCommand reports whether a message is a stop/cancel order for the agent,
// and returns any instruction that follows the stop word ("stop, check prod-a
// instead" → rest != ""; a bare "stop"/"please stop"/"cancel that" → ""). It is
// deliberately conservative: only messages that LEAD with a stop word count —
// "can you stop the dev cluster?" is a task, not an abort.
func stopCommand(text string) (isStop bool, rest string) {
	t := strings.ToLower(slackMention.ReplaceAllString(text, " "))
	fields := strings.FieldsFunc(t, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ',' || r == '.' || r == '!' || r == ';' || r == ':'
	})
	i := 0
	for i < len(fields) && (fields[i] == "please" || fields[i] == "ok" || fields[i] == "okay" || fields[i] == "hey" || fields[i] == "no") {
		i++
	}
	if i >= len(fields) {
		return false, ""
	}
	switch fields[i] {
	case "stop", "cancel", "abort", "halt":
	default:
		return false, ""
	}
	after := fields[i+1:]
	for len(after) > 0 {
		switch after[0] {
		case "please", "that", "it", "now", "everything", "all", "working", "this":
			after = after[1:]
		default:
			return true, strings.Join(after, " ")
		}
	}
	return true, ""
}

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
		"You are ONE PARTICIPANT in a team conversation, not its owner. The humans in the thread also talk to EACH OTHER: if the latest message is addressed to another person, is two people coordinating between themselves, or otherwise needs nothing from you, reply with exactly "+noReply+" (nothing else) and no message will be posted.",
		"SCOPE: do exactly what you were asked, then stop. An idea someone floats in passing (\"this could also help us consolidate X\") is context, NOT a task — do not start on it, do not draft a plan for it, and do not request credentials for it. If you think you could help with something nobody assigned you, offer it in ONE short sentence at most, and drop it unless someone explicitly says yes.",
		"Never begin multi-step work (inventories, audits, designs, migrations) on your own initiative. Say what you would do in a sentence or two and wait for an explicit go-ahead before doing any of it.",
		"Answer the specific question that was asked, concisely. Do not append capability menus, offers of other things you could look into, or plans for work beyond the question.",
		"If a message arrives mid-task (marked as a new Slack message in the tool results), deal with it FIRST — it may correct, narrow, or cancel what you're doing. A newer instruction always outranks your current plan.",
		"When a user says stop, your in-flight work is cancelled for you. Afterwards, acknowledge in ONE short line and wait for direction — do NOT resume or re-plan the cancelled work unless someone asks again.",
		"If a task you were asked to do needs a credential you don't have (a cloud key, an API token), DON'T give up — call the `request_secret` tool. It DMs the user a secure link to provide it, then writes it to a file in this container so you can use it.",
		"CREDENTIAL REQUESTS INTERRUPT HUMANS: request_secret DMs a user or pings the secret's approvers. Only call it when the credential is required for a task someone explicitly asked YOU to do — never speculatively, never \"in parallel\" for a side idea, and never just because a credential is listed as available.",
		"IMPORTANT: if you already requested a secret and the user now says they've added it / asks you to check, call `request_secret` AGAIN with the same name — that installs the value. Do NOT use bash to look for it; the value is only pulled into the container by request_secret.",
		"When the user wants a document they can take OUT of Slack — a design doc to hand to a coding agent, a runbook, a spec — write it as full Markdown (headers/tables are fine there; the Slack-formatting rule applies only to chat replies) and call `publish_document`. It returns a time-bound share link. In your reply, always give the link, state plainly when it expires (the tool tells you), and mention that saying \"reshare\" here gets a fresh link. If the user asks to reshare/regenerate a link, call `publish_document` again with that document's `artifact_id` — do NOT resend the content.",
		"Prefer read-only commands. This is a chat thread — you may be asked follow-up questions.")
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
			"path is returned. This tool is the ONLY way to get the value into the container; do NOT bash-check for it. " +
			"Every request interrupts a human (a DM or an approver ping): only call it when the credential is required for " +
			"a task a user explicitly asked you to do, never speculatively."),
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
	publishDocTool := anthropic.ToolParam{
		Name: "publish_document",
		Description: anthropic.String("Publish a Markdown document (a design doc, spec, runbook) and get back a TIME-BOUND " +
			"share link the user can hand to tools outside Slack (the URL serves the raw markdown). The result tells you the " +
			"exact expiry — always repeat the link AND its expiry in your reply, and note that the user can say \"reshare\" " +
			"in this thread for a fresh link. To reshare an expired/old link, call this again with the returned artifact_id " +
			"and NO markdown — the stored document is relinked as-is (documents are immutable; to change content, publish a " +
			"new document instead)."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"title":       map[string]any{"type": "string", "description": "short document title, e.g. \"Billing-alerts design\""},
				"markdown":    map[string]any{"type": "string", "description": "the full document as Markdown (omit when resharing via artifact_id)"},
				"artifact_id": map[string]any{"type": "string", "description": "OPTIONAL: id of a previously published document to mint a fresh link for (revokes its old links)"},
			},
			Required: []string{"title"},
		},
	}
	return &agentSession{
		client:        anthropic.NewClient(), // reads ANTHROPIC_API_KEY
		model:         model,
		sys:           sys,
		tools:         []anthropic.ToolUnionParam{{OfTool: &bashTool}, {OfTool: &reqSecretTool}, {OfTool: &publishDocTool}},
		controllerURL: os.Getenv("CLAW_CONTROLLER_URL"),
		runID:         os.Getenv("CLAW_RUN_ID"),
		agentName:     os.Getenv("CLAW_AGENT_NAME"),
	}
}

// loadAvailableSecrets appends the secrets the agent can request/retrieve (names
// + descriptions, never values) to the system prompt, so it uses an existing key
// by name instead of asking the user for a new one.
func (s *agentSession) loadAvailableSecrets(ctx context.Context) {
	if s.controllerURL == "" || s.currentRunID() == "" || os.Getenv("CLAW_TOKEN") == "" {
		return
	}
	url := fmt.Sprintf("%s/v1/runs/%s/available-secrets", s.controllerURL, s.currentRunID())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	authClawToken(req)
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
	if s.controllerURL == "" || sessionID == "" || os.Getenv("CLAW_TOKEN") == "" {
		return
	}
	url := fmt.Sprintf("%s/v1/sessions/%s/history", s.controllerURL, sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	authClawToken(req)
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
	// Turns with an empty output are messages that never got their own reply
	// (coalesced into a later answer, or judged to need none). The API requires
	// user/assistant alternation, so fold each unanswered input into the NEXT
	// answered turn's user message — mirroring how the live turn saw them.
	var pending []string
	for _, t := range out.Turns {
		if t.Output == "" {
			pending = append(pending, t.Input)
			continue
		}
		in := t.Input
		if len(pending) > 0 {
			in = strings.Join(append(pending, in), "\n\n")
			pending = nil
		}
		s.messages = append(s.messages,
			anthropic.NewUserMessage(anthropic.NewTextBlock(in)),
			anthropic.NewAssistantMessage(anthropic.NewTextBlock(t.Output)))
	}
	// Trailing unanswered messages have no answered turn to ride with; a
	// synthetic ack keeps the alternation valid without inventing an answer.
	if len(pending) > 0 {
		s.messages = append(s.messages,
			anthropic.NewUserMessage(anthropic.NewTextBlock(strings.Join(pending, "\n\n"))),
			anthropic.NewAssistantMessage(anthropic.NewTextBlock("(These messages were acknowledged without a direct reply.)")))
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

	// Everything in this turn — model calls, bash, approval waits, heartbeat —
	// runs under tctx so a user "stop" cancels it all at once. The watcher is
	// the turn's only claimer of queued messages. It runs under its OWN child
	// context so the settle path can quiesce it — stop it and WAIT for any
	// in-flight claim to finish — without cancelling the turn: a cancelled
	// claim HTTP request could commit server-side and orphan the run, and a
	// claim that lands after the final interrupt drain must re-enter the loop,
	// not ride out silently with a stale answer.
	s.beginTurn()
	tctx, cancelTurn := context.WithCancel(ctx)
	defer cancelTurn()
	var cancelWatcher context.CancelFunc
	var watcherDone chan struct{}
	startWatcher := func() {
		var wctx context.Context
		wctx, cancelWatcher = context.WithCancel(tctx)
		done := make(chan struct{})
		watcherDone = done
		go func() { defer close(done); s.watchInterrupts(wctx, cancelTurn) }()
	}
	quiesce := func() { cancelWatcher(); <-watcherDone }
	startWatcher()
	defer func() { quiesce() }()

	// Heartbeat: for turns that run long, post in-thread progress on a backoff
	// schedule summarizing what the agent has been doing, so a slow operation
	// isn't silent — and isn't a black box.
	s.resetActivity()
	go s.heartbeat(tctx)

	var final []string
	for { // the turn's ctx deadline (see main.go) bounds the agentic loop
		markCacheBreakpoint(s.messages)
		resp, err := s.callModel(tctx, anthropic.MessageNewParams{
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
			// A user stop cancelled tctx mid-call: close the turn cleanly with a
			// short acknowledgement instead of surfacing a cancellation error.
			if s.isAborted() {
				return s.finishAbort(), nil
			}
			return "", err
		}
		s.servedModel = string(resp.Model)
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
					result = s.requestSecret(tctx, in.Name, in.Description, in.Reason, in.EnvVar)
				case "publish_document":
					var in struct {
						Title      string `json:"title"`
						Markdown   string `json:"markdown"`
						ArtifactID string `json:"artifact_id"`
					}
					_ = json.Unmarshal(raw, &in)
					s.setStep("Publishing document \"" + in.Title + "\"…")
					result = s.publishDocument(tctx, in.Title, in.Markdown, in.ArtifactID)
				default: // bash
					var in struct {
						Command string `json:"command"`
					}
					_ = json.Unmarshal(raw, &in)
					// The running command is what the heartbeat reports — it's always
					// current, unlike narration text (which may be a conclusion).
					s.setStep("Running `" + firstLine([]byte(in.Command)) + "`…")
					result = runBash(tctx, in.Command)
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
			// Messages that arrived while tools ran ride along with the results, so
			// a correction ("actually, skip that") lands before more work happens.
			for _, t := range s.takeInterrupts() {
				toolResults = append(toolResults, anthropic.NewTextBlock(
					"[A new Slack message arrived while you were working — handle it before continuing; it may change or cancel the current task]\n"+t))
			}
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
		// The model finished — but if messages arrived in the meantime, this
		// answer may already be stale ("don't worry about that"). Hold it back,
		// show the model what just came in, and let it reply once, current.
		// (The draft stays in history, so nothing it found is lost.)
		injectStale := func(texts []string) {
			blocks := make([]anthropic.ContentBlockParamUnion, 0, len(texts))
			for _, t := range texts {
				blocks = append(blocks, anthropic.NewTextBlock(
					"[A new Slack message arrived before your reply above was posted — it was NOT posted. Write a fresh reply that answers this first and keeps only the still-relevant parts.]\n"+t))
			}
			s.messages = append(s.messages, anthropic.NewUserMessage(blocks...))
		}
		if texts := s.takeInterrupts(); len(texts) > 0 {
			injectStale(texts)
			continue
		}
		// Nothing pending — but the watcher may be MID-CLAIM right now, and a
		// message it lands after the drain above must not settle unseen.
		// Quiesce it, then re-check; only a drained-and-quiet turn may finish.
		quiesce()
		if s.isAborted() {
			return s.finishAbort(), nil
		}
		if texts := s.takeInterrupts(); len(texts) > 0 {
			injectStale(texts)
			startWatcher()
			continue
		}
		break
	}
	answer := strings.Join(final, "\n\n")
	if answer == "" {
		return "", fmt.Errorf("agent produced no text answer")
	}
	// The agent judged the message wasn't for it — suppress the reply entirely
	// (the sentinel is the whole answer; ignore any narration before it).
	if strings.TrimSpace(final[len(final)-1]) == noReply {
		return noReply, nil
	}
	// Tag the pod's first reply with the agent picked to service this thread and
	// the model actually served (per the API response, not the requested
	// constant). Re-appearing mid-thread = pod restart.
	if !s.modelTagged && s.servedModel != "" {
		if s.agentName != "" {
			answer += fmt.Sprintf("\n\n_agent: %s · model: %s_", s.agentName, s.servedModel)
		} else {
			answer += fmt.Sprintf("\n\n_model: %s_", s.servedModel)
		}
		s.modelTagged = true
	}
	return answer, nil
}

// finishAbort closes out a turn cancelled by a user stop. The history may end
// with tool calls whose results never arrived — the API rejects a conversation
// with dangling tool_use blocks, so each one gets a synthetic "(cancelled)"
// result — and a short acknowledgement becomes both the turn's answer and the
// assistant's last message, keeping history and Slack consistent.
func (s *agentSession) finishAbort() string {
	if n := len(s.messages); n > 0 && s.messages[n-1].Role == anthropic.MessageParamRoleAssistant {
		var results []anthropic.ContentBlockParamUnion
		for _, block := range s.messages[n-1].Content {
			if tu := block.OfToolUse; tu != nil {
				results = append(results, anthropic.NewToolResultBlock(tu.ID, "(cancelled — the user said stop)", false))
			}
		}
		if len(results) > 0 {
			s.messages = append(s.messages, anthropic.NewUserMessage(results...))
		}
	}
	const ack = "🛑 Stopped — I've cancelled what I was doing."
	s.messages = append(s.messages, anthropic.NewAssistantMessage(anthropic.NewTextBlock(ack)))
	return ack
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
		// The turn itself was cancelled (user stop / deadline) — not a model
		// failure. Bail without retries or a spurious "retrying…" post.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
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

// progressMessage builds the heartbeat text: what the agent is doing right now,
// plus a short recap of the steps it already took this turn — so a long task
// reads as a summary of work rather than a single opaque line.
func (s *agentSession) progressMessage() string {
	current, done := s.activitySummary()
	clip := func(t string) string {
		if len(t) > 200 {
			return t[:200] + "…"
		}
		return t
	}
	var b strings.Builder
	if current != "" {
		b.WriteString("⏳ " + clip(current))
	} else {
		b.WriteString("⏳ Still working on it…")
	}
	if len(done) > 0 {
		b.WriteString("\n_So far:_")
		for _, d := range done {
			b.WriteString("\n• " + clip(d))
		}
	}
	return b.String()
}

// heartbeat posts an in-thread progress update on an exponential-backoff schedule
// while a turn runs long, so slow operations report what they're doing instead of
// going silent — without spamming the thread on a genuinely long task. Intervals
// double from 1m: posts land at 1m, 2m, 4m, then every 4m after. The first post is
// at ~60s, so quick turns stay clean.
func (s *agentSession) heartbeat(ctx context.Context) {
	if s.controllerURL == "" || s.currentRunID() == "" {
		return
	}
	const (
		firstInterval = 60 * time.Second
		maxInterval   = 4 * time.Minute
	)
	interval := firstInterval
	lastPosted := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			// Never post the same update twice in a row: when nothing has changed
			// (e.g. a long wait on an approval), a repeated "On it. Let me…" reads
			// like the agent re-announcing the same work every minute.
			if msg := s.progressMessage(); msg != lastPosted {
				s.postProgress(ctx, msg)
				lastPosted = msg
			}
			if interval < maxInterval {
				if interval *= 2; interval > maxInterval {
					interval = maxInterval
				}
			}
		}
	}
}

// postProgress sends an intermediate, in-thread status message (best-effort).
func (s *agentSession) postProgress(ctx context.Context, text string) {
	body, _ := json.Marshal(map[string]string{"text": text})
	url := fmt.Sprintf("%s/v1/runs/%s/progress", s.controllerURL, s.currentRunID())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	authClawToken(req)
	if resp, e := http.DefaultClient.Do(req); e == nil {
		resp.Body.Close()
	}
}

// requestSecret asks the controller to collect a credential on demand: it DMs
// the user an intake link, then polls until the value is provided, writes it to
// the tmpfs secrets dir, and points $GOOGLE_APPLICATION_CREDENTIALS at it.
func (s *agentSession) requestSecret(ctx context.Context, name, description, reason, envVar string) string {
	if s.controllerURL == "" || s.currentRunID() == "" || os.Getenv("CLAW_TOKEN") == "" {
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
	if err := s.post(ctx, fmt.Sprintf("/v1/runs/%s/request-secret", s.currentRunID()), body); err != nil {
		return "Couldn't request the secret: " + err.Error()
	}
	// Poll briefly in case it's approved/provided right away.
	s.setStep("Waiting for access to *" + name + "* to be approved or provided…")
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if path, content, ok := s.fetchRequested(ctx, name); ok {
			return s.install(ctx, name, path, content, envVar)
		}
		// A new message outranks the wait: stop early so the model sees it now
		// instead of after the full poll (it may cancel this whole task).
		if s.hasInterrupts() {
			return fmt.Sprintf("Stopped waiting for *%s* early — a new Slack message just arrived (it follows this result); handle it first. The request is still open: call request_secret again with name=%q once it's approved/provided.", name, name)
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

// publishDocument stores a markdown document via the controller and returns a
// tool-result string carrying the share URL and its exact expiry, so the agent
// can (and is instructed to) relay both to the user. An artifactID reshares an
// already-published document under a fresh link, revoking the old ones.
func (s *agentSession) publishDocument(ctx context.Context, title, markdown, artifactID string) string {
	if s.controllerURL == "" || s.currentRunID() == "" || os.Getenv("CLAW_TOKEN") == "" {
		return "publish_document is unavailable in this run (no controller binding)."
	}
	body, _ := json.Marshal(map[string]string{"title": title, "content": markdown, "artifactId": artifactID})
	url := fmt.Sprintf("%s/v1/runs/%s/artifacts", s.controllerURL, s.currentRunID())
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "Couldn't publish the document: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	authClawToken(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "Couldn't publish the document: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && artifactID != "" {
		return fmt.Sprintf("No document with artifact_id %q exists in this conversation — publish it again with the full markdown instead.", artifactID)
	}
	if resp.StatusCode >= 300 {
		return "Couldn't publish the document: controller returned " + resp.Status
	}
	var out struct{ ArtifactID, URL, ExpiresAt string }
	if json.NewDecoder(resp.Body).Decode(&out) != nil || out.URL == "" {
		return "Couldn't publish the document: unexpected controller response."
	}
	expiry := out.ExpiresAt
	if t, e := time.Parse(time.RFC3339, out.ExpiresAt); e == nil {
		expiry = fmt.Sprintf("%s (in %s)", t.UTC().Format("Mon Jan 2, 15:04 MST"), time.Until(t).Round(time.Minute))
	}
	return fmt.Sprintf("Published %q.\nShare link: %s\nLink expires: %s\nartifact_id: %s (keep for resharing)\n"+
		"Give the user the link, tell them exactly when it expires, and that saying \"reshare\" here mints a fresh link.",
		title, out.URL, expiry, out.ArtifactID)
}

func (s *agentSession) post(ctx context.Context, path string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.controllerURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	authClawToken(req)
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
	url := fmt.Sprintf("%s/v1/runs/%s/requested-secret?name=%s", s.controllerURL, s.currentRunID(), name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", nil, false
	}
	authClawToken(req)
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
