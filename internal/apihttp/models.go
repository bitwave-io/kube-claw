package apihttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/traego/kube-claw/internal/store"
)

// Model registry endpoints (UI-managed multi-model support).
//
// Admin surface (basic-auth, like settings): CRUD + set-default. API keys are
// write-only — list responses carry hasKey, never the key.
// Runner surface (run-token): GET the session's resolved model (with the
// decrypted key — same trust envelope as secret materialization) + the
// registry names, and POST a session switch (the switch_model chat tool).

type modelView struct {
	store.Model
	HasKey bool `json:"hasKey"`
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeErr(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	list, err := s.Models.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]modelView, 0, len(list))
	for _, m := range list {
		out = append(out, modelView{Model: m, HasKey: len(m.APIKeyCiphertext) > 0})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) upsertModel(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeErr(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	var in struct {
		Name      string `json:"name"`
		Provider  string `json:"provider"`
		ModelID   string `json:"modelId"`
		BaseURL   string `json:"baseUrl"`
		APIKey    string `json:"apiKey"`
		MaxTokens int    `json:"maxTokens"`
		Notes     string `json:"notes"`
		Default   bool   `json:"default"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	m := store.Model{Name: in.Name, Provider: in.Provider, ModelID: in.ModelID, BaseURL: in.BaseURL, MaxTokens: in.MaxTokens, Notes: in.Notes}
	// Default assignment (explicit, or first-model-registered) happens inside
	// the service's transaction — the registry can never end up default-less.
	if err := s.Models.Upsert(r.Context(), m, in.APIKey, in.Default); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeErr(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	if err := s.Models.Delete(r.Context(), r.PathValue("name")); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) setDefaultModel(w http.ResponseWriter, r *http.Request) {
	if !s.adminOK(r) {
		writeErr(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	if err := s.Models.SetDefault(r.Context(), r.PathValue("name")); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// runModel returns the resolved model for the calling run's session, plus the
// registry names (for the switch_model tool's listing). 404 when the registry
// is empty — the runner falls back to its env config (legacy installs).
func (s *Server) runModel(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if !s.authRunInSession(r, runID) {
		writeErr(w, http.StatusUnauthorized, "invalid run token")
		return
	}
	session := s.runSession(r, runID)
	resolved, err := s.Models.Resolve(r.Context(), session)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "no models configured")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type choice struct {
		Name      string `json:"name"`
		Provider  string `json:"provider"`
		ModelID   string `json:"modelId"`
		Notes     string `json:"notes,omitempty"`
		IsDefault bool   `json:"isDefault"`
	}
	var available []choice
	if list, err := s.Models.List(r.Context()); err == nil {
		for _, m := range list {
			available = append(available, choice{Name: m.Name, Provider: m.Provider, ModelID: m.ModelID, Notes: m.Notes, IsDefault: m.IsDefault})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": resolved, "available": available})
}

// switchRunModel pins the calling run's session to a registered model.
func (s *Server) switchRunModel(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if !s.authRunInSession(r, runID) {
		writeErr(w, http.StatusUnauthorized, "invalid run token")
		return
	}
	session := s.runSession(r, runID)
	if session == "" {
		writeErr(w, http.StatusBadRequest, "run has no session — model switching needs a conversation")
		return
	}
	var in struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Model) == "" {
		writeErr(w, http.StatusBadRequest, "body must be {\"model\":\"<registered name>\"}")
		return
	}
	if err := s.Models.SetSessionModel(r.Context(), session, strings.TrimSpace(in.Model), runID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "no such model — call without a model to list the registry")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "model": strings.TrimSpace(in.Model)})
}

// runSession returns the session id of a run ("" if none).
func (s *Server) runSession(r *http.Request, runID string) string {
	session := ""
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if run, err := tx.GetRun(runID); err == nil {
			session = run.SessionID
		}
		return nil
	})
	return session
}
