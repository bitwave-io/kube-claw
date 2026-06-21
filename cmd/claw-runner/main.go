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

	response := respond(input)
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

	// Real GCP billing query, using the materialized credential. Works on a
	// gcloud-equipped base image with a real key; otherwise reports why not.
	if res, err := gcloudCostQuery(); err == nil {
		parts = append(parts, "GCP billing accounts: "+res)
	} else {
		parts = append(parts, "gcloud: "+err.Error())
	}

	return strings.Join(parts, " | ")
}

// gcloudCostQuery authenticates with the materialized service-account key and
// runs a read-only billing query — the real agent action.
func gcloudCostQuery() (string, error) {
	gc, err := exec.LookPath("gcloud")
	if err != nil {
		return "", fmt.Errorf("not in image (use the gcloud base image)")
	}
	cred := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if cred == "" {
		return "", fmt.Errorf("no GOOGLE_APPLICATION_CREDENTIALS")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if out, err := exec.CommandContext(ctx, gc, "auth", "activate-service-account",
		"--key-file="+cred).CombinedOutput(); err != nil {
		return "", fmt.Errorf("auth failed: %s", firstLine(out))
	}
	out, err := exec.CommandContext(ctx, gc, "billing", "accounts", "list",
		"--format=value(displayName,open)").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("billing query failed: %s", firstLine(out))
	}
	res := strings.TrimSpace(string(out))
	if res == "" {
		res = "(none visible to this key)"
	}
	return res, nil
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
