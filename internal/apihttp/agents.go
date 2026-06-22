package apihttp

import (
	"encoding/json"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
)

// createAgentReq is the friendly agent-registration shape. The handler builds
// the Agent CRD from it so callers never hand-write YAML.
type createAgentReq struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	BaseImageRef string `json:"baseImageRef"`
	Image        string `json:"image"`
	SystemPrompt string `json:"systemPrompt"`
	IdleTimeout  string `json:"idleTimeout"`
	Secrets      []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Env  string `json:"env"`
	} `json:"secrets"`
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	var req createAgentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Namespace == "" {
		req.Namespace = "claw-agents"
	}
	if req.BaseImageRef == "" && req.Image == "" {
		writeErr(w, http.StatusBadRequest, "baseImageRef or image is required")
		return
	}

	agent := &clawv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: req.Namespace},
		Spec: clawv1alpha1.AgentSpec{
			BaseImageRef: req.BaseImageRef,
			Image:        req.Image,
			Runtime:      clawv1alpha1.RuntimeSpec{Mode: "scaleToZeroSession", IdleTimeout: orDefault(req.IdleTimeout, "15m")},
		},
	}
	if req.SystemPrompt != "" {
		agent.Spec.Model = &clawv1alpha1.ModelSpec{SystemPrompt: req.SystemPrompt}
	}
	for _, sec := range req.Secrets {
		d := clawv1alpha1.DeliverySpec{Type: "file", Path: sec.Path, Mode: "0400"}
		if sec.Env != "" && sec.Path != "" {
			d.Env = map[string]string{sec.Env: sec.Path}
		}
		agent.Spec.Secrets = append(agent.Spec.Secrets, clawv1alpha1.SecretRef{Name: sec.Name, Delivery: d})
	}

	if err := s.K8s.Create(r.Context(), agent); apierrors.IsAlreadyExists(err) {
		writeErr(w, http.StatusConflict, "agent already exists")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name, "namespace": req.Namespace, "status": "created"})
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
