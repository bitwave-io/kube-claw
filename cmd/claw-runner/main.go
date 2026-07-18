// Command claw-runner is the default reference agent runner. It is launched by
// the run engine as a one-shot Job, honors the CLAW_* env contract (DESIGN.md
// §11, §36), produces a response, and POSTs it back to the controller.
//
// Phase 5 demo slice: the response is a simple stub. A real runner would call an
// LLM and use materialized secrets. Secrets/login are not wired here yet.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	runID := os.Getenv("CLAW_RUN_ID")
	controllerURL := os.Getenv("CLAW_CONTROLLER_URL")
	input := os.Getenv("CLAW_INPUT")
	if runID == "" || controllerURL == "" {
		fmt.Fprintln(os.Stderr, "claw-runner: CLAW_RUN_ID and CLAW_CONTROLLER_URL are required")
		os.Exit(2)
	}

	// Without a key, the stub still proves the materialize→respond path.
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		response := respond(input)
		fmt.Printf("claw-runner: run=%s input=%q -> %q\n", runID, input, response)
		if err := postOutput(controllerURL, runID, response); err != nil {
			fmt.Fprintf(os.Stderr, "claw-runner: posting output: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Real agent loop. Turn 1 is CLAW_INPUT; then, for a Slack session, stay warm
	// and claim follow-up turns until the idle timeout (the pod scales to zero).
	sess := newAgentSession(os.Getenv("CLAW_SYSTEM_PROMPT"))
	// Cold-start replay: if this pod is serving a follow-up in an existing thread
	// (the warm pod idled out), seed the conversation from the store.
	if sid := os.Getenv("CLAW_SESSION_ID"); sid != "" {
		hctx, hcancel := context.WithTimeout(context.Background(), 20*time.Second)
		sess.loadHistory(hctx, sid)
		hcancel()
	}
	// Tell the agent which credentials are already registered (names only).
	actx, acancel := context.WithTimeout(context.Background(), 15*time.Second)
	sess.loadAvailableSecrets(actx)
	acancel()
	// For a Slack session, let a running turn claim messages that arrive mid-turn
	// (interrupts): the model sees them immediately instead of after it finishes.
	sessionID := os.Getenv("CLAW_SESSION_ID")
	if sessionID != "" {
		pod := os.Getenv("HOSTNAME")
		sess.claimTurn = func() (string, string, bool) { return claimNextTurn(controllerURL, sessionID, pod) }
	}
	answer := turn(sess, runID, input)
	if err := deliver(sess, controllerURL, runID, answer); err != nil {
		fmt.Fprintf(os.Stderr, "claw-runner: posting output: %v\n", err)
		os.Exit(1)
	}

	if sessionID == "" {
		fmt.Println("claw-runner: no session — exiting 0")
		return
	}
	warmLoop(sess, controllerURL, sessionID)
}

// deliver posts a finished turn's answer. If the turn absorbed messages that
// arrived mid-run, the answer posts on the NEWEST of those runs (whose 👀 is
// the one still pending in Slack) and the older runs complete silently — one
// current reply instead of a stale one per message.
func deliver(sess *agentSession, controllerURL, runID, answer string) error {
	ids := append([]string{runID}, sess.takeExtraRuns()...)
	for _, id := range ids[:len(ids)-1] {
		if err := postOutput(controllerURL, id, noReply); err != nil {
			fmt.Fprintf(os.Stderr, "claw-runner: closing absorbed run %s: %v\n", id, err)
		}
	}
	return postOutput(controllerURL, ids[len(ids)-1], answer)
}

// turn runs one message to an answer string (errors become a clear, visible
// reply). The runner retries transient failures internally (with in-thread
// "retrying…" messages); this is the final, humanized message once it gives up.
func turn(sess *agentSession, runID, input string) string {
	// Attribute this turn's callbacks (progress, secret requests, published
	// artifacts) to the run being served, not the pod's first run.
	sess.setRunID(runID)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	ans, err := sess.turn(ctx, input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claw-runner: agent turn failed: %v\n", err)
		return humanizeErr(err)
	}
	// A stop that carried a new instruction ("stop, check prod-a instead")
	// cancels the old work first (done inside sess.turn), then the instruction
	// runs as a fresh turn — its answer supersedes the bare stop ack.
	if follow := sess.takeStopFollowup(); follow != "" {
		return turn(sess, runID,
			"[You were stopped mid-task and that work is cancelled — do not resume it. The stopping message also contained a new instruction; briefly confirm the stop, then handle it:]\n"+follow)
	}
	fmt.Printf("claw-runner: run=%s input=%q -> %q\n", runID, input, ans)
	return ans
}

// humanizeErr turns a raw failure into a clear message for the user.
func humanizeErr(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "no such host"), strings.Contains(s, "dial tcp"),
		strings.Contains(s, "lookup "), strings.Contains(s, "connection refused"),
		strings.Contains(s, "connection reset"):
		return "⚠️ I kept losing my connection to the model (a network/DNS problem) and couldn't finish, even after retrying. It looks transient — please ask again in a moment."
	case strings.Contains(s, "deadline"), strings.Contains(s, "timeout"), strings.Contains(s, "context canceled"):
		return "⚠️ That took too long and I had to stop before finishing. Please try again, or narrow the request."
	case strings.Contains(s, "rate"), strings.Contains(s, "429"), strings.Contains(s, "overloaded"), strings.Contains(s, "529"):
		return "⚠️ The model is rate-limited/overloaded right now and I couldn't complete that after retrying. Please try again shortly."
	default:
		return "⚠️ I ran into an error and couldn't complete that: " + firstLine([]byte(err.Error()))
	}
}

// warmLoop keeps the pod alive, claiming follow-up turns for this Slack thread
// until idle for CLAW_IDLE_TIMEOUT (reset on each turn — the "bumpable" timeout).
func warmLoop(sess *agentSession, controllerURL, sessionID string) {
	idle := parseDurationOr(os.Getenv("CLAW_IDLE_TIMEOUT"), 5*time.Minute)
	pod := os.Getenv("HOSTNAME")
	fmt.Printf("claw-runner: warm session %s (idle timeout %s)\n", sessionID, idle)
	lastActivity := time.Now()
	for {
		runID, input, ok := claimNextTurn(controllerURL, sessionID, pod)
		if ok {
			// Drain the backlog before answering: messages that piled up while the
			// last turn ran are answered ONCE, newest instruction included — the
			// later message is often a correction of the earlier one.
			ids, texts := []string{runID}, []string{input}
			for {
				id, text, more := claimNextTurn(controllerURL, sessionID, pod)
				if !more {
					break
				}
				ids = append(ids, id)
				texts = append(texts, text)
			}
			for _, id := range ids[:len(ids)-1] {
				if err := postOutput(controllerURL, id, noReply); err != nil {
					fmt.Fprintf(os.Stderr, "claw-runner: closing coalesced run %s: %v\n", id, err)
				}
			}
			runID = ids[len(ids)-1]
			if len(texts) > 1 {
				input = "Several messages arrived while you were away — read them all before answering; the newest may change or cancel the earlier asks:\n\n" +
					strings.Join(texts, "\n\n")
			} else if stop, rest := stopCommand(input); stop && rest == "" {
				// A bare "stop" with nothing running needs no model call — the user
				// likely thinks work is still in flight. Confirm and move on.
				if err := postOutput(controllerURL, runID, "🛑 Nothing is running — I've stopped."); err != nil {
					fmt.Fprintf(os.Stderr, "claw-runner: posting stop ack: %v\n", err)
				}
				lastActivity = time.Now()
				continue
			}
			ans := turn(sess, runID, input)
			if err := deliver(sess, controllerURL, runID, ans); err != nil {
				fmt.Fprintf(os.Stderr, "claw-runner: posting follow-up output: %v\n", err)
			}
			lastActivity = time.Now() // bump the idle timer
			continue
		}
		if time.Since(lastActivity) >= idle {
			fmt.Println("claw-runner: idle timeout reached — scaling to zero, exiting 0")
			signalSleep(controllerURL, sessionID)
			return
		}
		time.Sleep(3 * time.Second)
	}
}

func parseDurationOr(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

// signalSleep tells the controller the session has gone idle so it can mark the
// thread's top-level message with 💤 (best-effort).
func signalSleep(controllerURL, sessionID string) {
	url := fmt.Sprintf("%s/v1/sessions/%s/sleep", controllerURL, sessionID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if resp, err := authedDo(ctx, http.MethodPost, url, nil); err == nil {
		resp.Body.Close()
	}
}

// claimNextTurn claims the next pending follow-up turn for this session, if any.
func claimNextTurn(controllerURL, sessionID, pod string) (runID, input string, ok bool) {
	url := fmt.Sprintf("%s/v1/sessions/%s/claim-next?pod=%s", controllerURL, sessionID, pod)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := authedDo(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNoContent:
		return "", "", false // nothing pending
	default:
		// authedDo already renewed + retried on 401; a status landing here is
		// abnormal — say so, never silently swallow it (a deaf warm pod blocks
		// the whole session while the engine defers pending turns to it).
		fmt.Fprintf(os.Stderr, "claw-runner: claim-next returned %s\n", resp.Status)
		return "", "", false
	}
	var out struct{ RunID, Input string }
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return "", "", false
	}
	return out.RunID, out.Input, true
}

// respond is the stub "agent". It proves the materialized credential is present
// (size only — NEVER the content), reports the secret's description (usage
// context the agent's LLM would use), and reports whether gcloud is available in
// the base image. Real GCP calls are a small next step from here.
func respond(input string) string {
	if input == "" {
		input = "(no question provided)"
	}

	var parts []string
	parts = append(parts, fmt.Sprintf("Demo response: I received %q.", input))

	// Credential presence (size only).
	if p := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			parts = append(parts, fmt.Sprintf("credential present at %s (%d bytes)", p, len(b)))
		}
	}

	// Secret descriptions from the bootstrap manifest (usage context).
	for _, d := range manifestDescriptions() {
		if d != "" {
			parts = append(parts, "secret usage: "+d)
		}
	}

	// Real cloud query using the materialized credential. The base image
	// determines which CLI is present (gcloud / aws / az); otherwise reports why not.
	if provider, res, err := cloudQuery(); err == nil {
		parts = append(parts, provider+": "+res)
	} else {
		parts = append(parts, "cloud: "+err.Error())
	}

	return strings.Join(parts, " | ")
}

// cloudQuery dispatches to whichever cloud CLI the base image provides and runs
// a read-only check using the materialized credential — the real agent action.
func cloudQuery() (provider, result string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	switch {
	case haveCLI("gcloud"):
		r, e := gcloudQuery(ctx)
		return "gcp billing accounts", r, e
	case haveCLI("aws"):
		r, e := awsQuery(ctx)
		return "aws identity", r, e
	case haveCLI("az"):
		r, e := azQuery(ctx)
		return "azure account", r, e
	default:
		return "", "", fmt.Errorf("no cloud CLI in image (use a gcloud/aws/azure base)")
	}
}

func haveCLI(name string) bool { _, err := exec.LookPath(name); return err == nil }

func gcloudQuery(ctx context.Context) (string, error) {
	cred := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if cred == "" {
		return "", fmt.Errorf("no GOOGLE_APPLICATION_CREDENTIALS")
	}
	if out, err := exec.CommandContext(ctx, "gcloud", "auth", "activate-service-account", "--key-file="+cred).CombinedOutput(); err != nil {
		return "", fmt.Errorf("auth failed: %s", firstLine(out))
	}
	out, err := exec.CommandContext(ctx, "gcloud", "billing", "accounts", "list", "--format=value(displayName,open)").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("billing query failed: %s", firstLine(out))
	}
	return nonEmpty(out), nil
}

func awsQuery(ctx context.Context) (string, error) {
	// AWS picks up creds from env (AWS_ACCESS_KEY_ID/...) or a shared file the
	// secret delivery mounted. sts get-caller-identity is a safe read.
	out, err := exec.CommandContext(ctx, "aws", "sts", "get-caller-identity", "--output", "text").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("sts failed: %s", firstLine(out))
	}
	return nonEmpty(out), nil
}

func azQuery(ctx context.Context) (string, error) {
	// Azure auth (service principal) is set up by the runner/secret in a real
	// deployment; here we report the signed-in account if available.
	out, err := exec.CommandContext(ctx, "az", "account", "show", "--query", "name", "-o", "tsv").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("account show failed: %s", firstLine(out))
	}
	return nonEmpty(out), nil
}

func nonEmpty(b []byte) string {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "(none)"
	}
	return s
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// manifestDescriptions reads the bootstrap-written manifest (names+descriptions,
// never values) so the agent knows what each materialized secret is for.
func manifestDescriptions() []string {
	dir := os.Getenv("CLAW_SECRETS_DIR")
	if dir == "" {
		dir = "/var/run/claw/secrets"
	}
	b, err := os.ReadFile(filepath.Join(dir, ".claw-manifest.json"))
	if err != nil {
		return nil
	}
	var entries []struct {
		Name, Description, Path string
	}
	if json.Unmarshal(b, &entries) != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Description)
	}
	return out
}

func postOutput(controllerURL, runID, content string) error {
	// A noReply answer still completes the run (and clears its 👀 marker in
	// Slack) but the controller posts nothing to the thread.
	kind := "text"
	if strings.TrimSpace(content) == noReply {
		kind, content = "none", ""
	}
	body, _ := json.Marshal(map[string]string{"kind": kind, "content": content})
	url := fmt.Sprintf("%s/v1/runs/%s/outputs", controllerURL, runID)

	// authedDo renews the token and retries once on 401: an answer produced
	// just as the access token expires must not be lost (the controller dedupes
	// replays, so retrying is safe).
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := authedDo(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("controller returned %s", resp.Status)
	}
	return nil
}

// authClawToken adds the run session token so the controller can authenticate
// runner callbacks. The token manager (token.go) keeps it fresh — seeded by
// claw-bootstrap, renewed via refresh token or SA re-login before it expires.
func authClawToken(req *http.Request) {
	if t := clawToken(os.Getenv("CLAW_CONTROLLER_URL")); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
}
