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
// (size only — NEVER the content) without doing real GCP calls yet.
func respond(input string) string {
	if input == "" {
		input = "(no question provided)"
	}
	cred := ""
	if p := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			cred = fmt.Sprintf(" [credential materialized at %s: %d bytes]", p, len(b))
		} else {
			cred = fmt.Sprintf(" [credential expected at %s but unreadable: %v]", p, err)
		}
	}
	return fmt.Sprintf("Demo response: I received %q.%s (stub runner — no real GCP calls yet.)", input, cred)
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
