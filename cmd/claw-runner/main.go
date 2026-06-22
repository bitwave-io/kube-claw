// Command claw-runner is the default reference agent runner. It is launched by
// the run engine as a one-shot Job, honors the CLAW_* env contract (DESIGN.md
// §11, §36), produces a response, and POSTs it back to the controller.
//
// Phase 5 demo slice: the response is a simple stub. A real runner would call an
// LLM and use materialized secrets. Secrets/login are not wired here yet.
package main

import (
	"bytes"
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

	// Real agent loop when an Anthropic key is present; otherwise the stub so
	// local/no-key runs still prove the materialize→respond path.
	var response string
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		ans, err := runAgent(ctx, os.Getenv("CLAW_SYSTEM_PROMPT"), input)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "claw-runner: agent loop failed: %v\n", err)
			response = "Agent error: " + err.Error()
		} else {
			response = ans
		}
	} else {
		response = respond(input)
	}
	fmt.Printf("claw-runner: run=%s input=%q -> %q\n", runID, input, response)

	if err := postOutput(controllerURL, runID, response); err != nil {
		fmt.Fprintf(os.Stderr, "claw-runner: posting output: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("claw-runner: output recorded, exiting 0")
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
	body, _ := json.Marshal(map[string]string{"kind": "text", "content": content})
	url := fmt.Sprintf("%s/v1/runs/%s/outputs", controllerURL, runID)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
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
