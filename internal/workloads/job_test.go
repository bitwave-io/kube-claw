package workloads

import (
	"testing"

	"github.com/traego/kube-claw/internal/store"
)

func TestBuildRunJob(t *testing.T) {
	run := store.Run{ID: "run-abc", AgentName: "gcp-cost", AgentNamespace: "claw-agents"}
	job := BuildRunJob(run, "claw-runner:dev", "http://claw-controller.claw-system.svc:8443", "why did cost spike?", "You are a cost bot.", "claw-anthropic-key")

	if job.Name != "run-abc" || job.Namespace != "claw-agents" {
		t.Fatalf("job name/ns = %s/%s", job.Name, job.Namespace)
	}
	pod := job.Spec.Template.Spec
	if pod.ServiceAccountName != "claw-agent-gcp-cost" {
		t.Errorf("SA = %s, want claw-agent-gcp-cost", pod.ServiceAccountName)
	}
	if pod.RestartPolicy != "Never" {
		t.Errorf("restartPolicy = %s, want Never", pod.RestartPolicy)
	}
	if len(pod.Containers) != 1 || pod.Containers[0].Image != "claw-runner:dev" {
		t.Fatalf("container image wrong: %+v", pod.Containers)
	}
	env := map[string]string{}
	for _, e := range pod.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["CLAW_RUN_ID"] != "run-abc" || env["CLAW_INPUT"] != "why did cost spike?" {
		t.Errorf("env = %v", env)
	}
	if env["CLAW_SYSTEM_PROMPT"] != "You are a cost bot." {
		t.Errorf("CLAW_SYSTEM_PROMPT = %q", env["CLAW_SYSTEM_PROMPT"])
	}
	var hasKey bool
	for _, e := range pod.Containers[0].Env {
		if e.Name == "ANTHROPIC_API_KEY" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			hasKey = e.ValueFrom.SecretKeyRef.Name == "claw-anthropic-key"
		}
	}
	if !hasKey {
		t.Error("expected ANTHROPIC_API_KEY from secret claw-anthropic-key")
	}
	if env["CLAW_CONTROLLER_URL"] == "" {
		t.Error("CLAW_CONTROLLER_URL not set")
	}
	// Security baseline.
	sc := pod.Containers[0].SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("expected readOnlyRootFilesystem=true")
	}
}
