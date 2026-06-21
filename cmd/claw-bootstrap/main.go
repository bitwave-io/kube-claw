// Command claw-bootstrap is the agent pod entrypoint. It performs the /login
// token exchange, materializes approved secrets to tmpfs, execs the runner as a
// subprocess (passing through its exit code), and wipes the tmpfs on exit
// (DESIGN.md §9, §11).
//
// Contract (env): CLAW_RUN_ID, CLAW_CONTROLLER_URL, CLAW_SECRETS_DIR,
// CLAW_SA_TOKEN_FILE. Args after the binary are the runner command.
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
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func main() {
	runID := os.Getenv("CLAW_RUN_ID")
	ctrl := os.Getenv("CLAW_CONTROLLER_URL")
	secretsDir := envOr("CLAW_SECRETS_DIR", "/var/run/claw/secrets")
	tokenFile := envOr("CLAW_SA_TOKEN_FILE", "/var/run/claw/sa-token/token")
	runnerCmd := os.Args[1:]

	if runID == "" || ctrl == "" || len(runnerCmd) == 0 {
		fatal("CLAW_RUN_ID, CLAW_CONTROLLER_URL and a runner command are required")
	}

	saToken, err := os.ReadFile(tokenFile)
	if err != nil {
		fatal(fmt.Sprintf("read SA token: %v", err))
	}

	clawTok, err := login(ctrl, runID, string(bytes.TrimSpace(saToken)))
	if err != nil {
		fatal(fmt.Sprintf("login: %v", err))
	}
	mats, err := materialize(ctrl, runID, clawTok)
	if err != nil {
		fatal(fmt.Sprintf("materialize: %v", err))
	}

	env := append(os.Environ(), "CLAW_TOKEN="+clawTok)
	for _, m := range mats {
		if err := writeSecret(m, secretsDir); err != nil {
			fatal(fmt.Sprintf("write secret %s: %v", m.Name, err))
		}
		for k, v := range m.Env {
			env = append(env, k+"="+v)
		}
	}
	fmt.Printf("claw-bootstrap: logged in, materialized %d secret(s), exec %v\n", len(mats), runnerCmd)

	code := runChild(runnerCmd, env)
	wipe(secretsDir)
	os.Exit(code)
}

func runChild(argv []string, env []string) int {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "claw-bootstrap: start runner: %v\n", err)
		return 1
	}
	// Relay termination signals to the child (PID-1 hygiene).
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 1
}

type matSecret struct {
	Name    string            `json:"name"`
	Path    string            `json:"path"`
	Mode    string            `json:"mode"`
	Env     map[string]string `json:"env"`
	Content string            `json:"content"`
}

func login(ctrl, runID, saToken string) (string, error) {
	body, _ := json.Marshal(map[string]string{"token": saToken, "runId": runID})
	var out struct {
		Token string `json:"token"`
	}
	if err := postJSON(ctrl+"/v1/login", "", body, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("empty session token")
	}
	return out.Token, nil
}

func materialize(ctrl, runID, clawTok string) ([]matSecret, error) {
	var out struct {
		Secrets []matSecret `json:"secrets"`
	}
	if err := postJSON(ctrl+"/v1/runs/"+runID+"/materialize", clawTok, nil, &out); err != nil {
		return nil, err
	}
	return out.Secrets, nil
}

func writeSecret(m matSecret, dir string) error {
	data, err := base64.StdEncoding.DecodeString(m.Content)
	if err != nil {
		return err
	}
	path := m.Path
	if path == "" {
		path = filepath.Join(dir, m.Name)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	mode := os.FileMode(0o400)
	if m.Mode != "" {
		if parsed, err := strconv.ParseUint(m.Mode, 8, 32); err == nil {
			mode = os.FileMode(parsed)
		}
	}
	return os.WriteFile(path, data, mode)
}

func wipe(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

func postJSON(url, bearer string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var rdr *bytes.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	} else {
		rdr = bytes.NewReader([]byte{})
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s -> %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "claw-bootstrap:", msg)
	os.Exit(1)
}
