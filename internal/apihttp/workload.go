package apihttp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
)

type loginReq struct {
	Token string `json:"token"`
	RunID string `json:"runId"`
}

// login is the /login token exchange (DESIGN.md §9). It verifies the platform
// credential, confirms the calling pod is a kube-claw pod for this run (pod
// identity from the token's bound claims), and issues a scoped claw session token.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" || req.RunID == "" {
		writeErr(w, http.StatusBadRequest, "token and runId are required")
		return
	}
	lg := logf.Log.WithName("login").WithValues("run", req.RunID)

	principal, err := s.Identity.Verify(r.Context(), req.Token)
	if err != nil {
		s.auditLoginFail(r.Context(), req.RunID, "verify: "+err.Error())
		writeErr(w, http.StatusUnauthorized, "credential verification failed")
		return
	}

	// Load the run and the pod the token is bound to, and check they line up.
	var run store.Run
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetRun(req.RunID)
		run = got
		return e
	}); err != nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}

	var pod corev1.Pod
	if err := s.Reader.Get(r.Context(), client.ObjectKey{Namespace: principal.Namespace, Name: principal.PodName}, &pod); err != nil {
		s.auditLoginFail(r.Context(), req.RunID, "pod load: "+err.Error())
		writeErr(w, http.StatusUnauthorized, "attestation failed")
		return
	}
	if reason := attestPod(&pod, principal.PodUID, run); reason != "" {
		s.auditLoginFail(r.Context(), req.RunID, reason)
		writeErr(w, http.StatusUnauthorized, "attestation failed")
		return
	}

	// Which secrets does this run currently have valid grants for?
	allowed, err := s.grantedSecretNames(r.Context(), run)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	tok, err := s.Signer.Issue(req.RunID, allowed, 10*time.Minute)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		return tx.AppendAudit(store.AuditEvent{Type: "workload.login", RunID: req.RunID, Actor: principal.ServiceAccount})
	})
	lg.Info("login ok", "pod", principal.PodName, "secrets", len(allowed))
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "secrets": allowed})
}

// attestPod verifies the pod is a kube-claw run pod for this run. Returns "" on
// success or a reason string on failure (DESIGN.md §9 checks).
func attestPod(pod *corev1.Pod, tokenPodUID string, run store.Run) string {
	if string(pod.UID) != tokenPodUID {
		return "pod uid mismatch (token vs pod)"
	}
	if pod.Namespace != run.AgentNamespace {
		return "pod namespace mismatch"
	}
	if pod.Labels["claw.run/run-id"] != run.ID {
		return "pod is not labelled for this run"
	}
	if pod.Spec.ServiceAccountName != "claw-agent-"+run.AgentName {
		return "pod service account mismatch"
	}
	return ""
}

func (s *Server) grantedSecretNames(ctx context.Context, run store.Run) ([]string, error) {
	var agent clawv1alpha1.Agent
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: run.AgentNamespace, Name: run.AgentName}, &agent); err != nil {
		return nil, fmt.Errorf("load agent: %w", err)
	}
	var allowed []string
	err := s.Store.Tx(ctx, func(tx store.Tx) error {
		for _, sref := range agent.Spec.Secrets {
			sec, err := tx.GetSecret(agent.Namespace, sref.Name)
			if err != nil {
				continue
			}
			dHash := secrets.DeliveryHash(sref.Delivery.Path, sref.Delivery.Mode, sref.Delivery.Env)
			if _, gerr := tx.FindValidGrant(agent.Namespace, agent.Name, sec.ID,
				agent.Status.SelectedImageDigest, agent.Status.AgentSpecHash, dHash); gerr == nil {
				allowed = append(allowed, sref.Name)
			}
		}
		return nil
	})
	return allowed, err
}

func (s *Server) auditLoginFail(ctx context.Context, runID, reason string) {
	_ = s.Store.Tx(ctx, func(tx store.Tx) error {
		return tx.AppendAudit(store.AuditEvent{Type: "workload.login_failed", RunID: runID, Detail: map[string]any{"reason": reason}})
	})
}

// materializeSecret is one secret payload returned to the bootstrap.
type materializeSecret struct {
	Name    string            `json:"name"`
	Path    string            `json:"path"`
	Mode    string            `json:"mode"`
	Env     map[string]string `json:"env"`
	Content string            `json:"content"` // base64 of plaintext
}

// materialize returns the approved, decrypted secret payloads for a run. Authn
// is the claw session token issued by /login (bearer), scoped to this run.
func (s *Server) materialize(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	claims, err := s.Signer.Verify(bearer(r))
	if err != nil || claims.RunID != runID {
		writeErr(w, http.StatusUnauthorized, "invalid session token")
		return
	}

	var run store.Run
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetRun(runID)
		run = got
		return e
	}); err != nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	var agent clawv1alpha1.Agent
	if err := s.Reader.Get(r.Context(), client.ObjectKey{Namespace: run.AgentNamespace, Name: run.AgentName}, &agent); err != nil {
		writeErr(w, http.StatusInternalServerError, "load agent")
		return
	}

	out := []materializeSecret{}
	for _, sref := range agent.Spec.Secrets {
		if !claims.Allows(sref.Name) {
			continue // token not scoped to this secret
		}
		val, err := s.Secrets.GetValue(r.Context(), agent.Namespace, sref.Name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "materialize "+sref.Name)
			return
		}
		out = append(out, materializeSecret{
			Name: sref.Name, Path: sref.Delivery.Path, Mode: sref.Delivery.Mode,
			Env: sref.Delivery.Env, Content: base64.StdEncoding.EncodeToString(val),
		})
		_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
			return tx.AppendAudit(store.AuditEvent{Type: "secret.materialized", RunID: runID, Detail: map[string]any{"secret": sref.Name}})
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": out})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	return strings.TrimPrefix(h, "Bearer ")
}
