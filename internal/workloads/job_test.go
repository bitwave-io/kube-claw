package workloads

import (
	"testing"

	"github.com/traego/kube-claw/internal/store"
)

func TestBuildRunJob(t *testing.T) {
	run := store.Run{ID: "run-abc", AgentName: "gcp-cost", AgentNamespace: "claw-agents", SessionID: "1782140687.401159"}
	job := BuildRunJob(run, "claw-runner:dev", "http://claw-controller.claw-system.svc:8443", "why did cost spike?", "You are a cost bot.", "claw-anthropic-key", "10m")

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
	if env["CLAW_IDLE_TIMEOUT"] != "10m" || env["CLAW_SESSION_ID"] != "1782140687.401159" {
		t.Errorf("idle/session env = %q / %q", env["CLAW_IDLE_TIMEOUT"], env["CLAW_SESSION_ID"])
	}
	if job.Labels["claw.run/session"] != "1782140687-401159" {
		t.Errorf("session label = %q, want 1782140687-401159", job.Labels["claw.run/session"])
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
	// Security baseline: non-root, no privilege escalation, all caps dropped. The
	// rootfs is writable (so the agent can install tooling at runtime) — the pod
	// sandbox is the boundary, not a read-only fs.
	sc := pod.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("expected a container SecurityContext")
	}
	if sc.ReadOnlyRootFilesystem == nil || *sc.ReadOnlyRootFilesystem {
		t.Error("expected readOnlyRootFilesystem=false (writable rootfs for runtime installs)")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("expected allowPrivilegeEscalation=false")
	}
	if sc.Capabilities == nil || len(sc.Capabilities.Drop) == 0 || sc.Capabilities.Drop[0] != "ALL" {
		t.Error("expected all capabilities dropped")
	}
	// Pod-level: non-root with an explicit uid + matching FSGroup so EmptyDir
	// volumes (incl. the tmpfs secrets dir) are writable by the agent user.
	psc := pod.SecurityContext
	if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Error("expected runAsNonRoot=true")
	}
	if psc == nil || psc.RunAsUser == nil || *psc.RunAsUser != 65532 {
		t.Error("expected runAsUser=65532")
	}
	if psc == nil || psc.FSGroup == nil || *psc.FSGroup != 65532 {
		t.Error("expected fsGroup=65532 so the secrets volume is writable")
	}
}
