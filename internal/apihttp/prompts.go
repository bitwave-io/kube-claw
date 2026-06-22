package apihttp

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"

	"github.com/traego/kube-claw/internal/store"
)

type setPromptReq struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Content   string `json:"content"`
}

func (s *Server) setPrompt(w http.ResponseWriter, r *http.Request) {
	var req setPromptReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" || req.Content == "" {
		writeErr(w, http.StatusBadRequest, "name and content are required")
		return
	}
	if req.Namespace == "" {
		req.Namespace = "claw-agents"
	}
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		return tx.SetPrompt(store.Prompt{AgentNamespace: req.Namespace, AgentName: req.Name, Content: req.Content})
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"namespace": req.Namespace, "name": req.Name, "status": "saved"})
}

func (s *Server) getPrompt(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	var p store.Prompt
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetPrompt(ns, name)
		p = got
		return e
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "prompt not found")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) listPrompts(w http.ResponseWriter, r *http.Request) {
	var ps []store.Prompt
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListPrompts()
		ps = got
		return e
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

// promptsPage renders the editable prompt list (one textarea per agent + a form
// to add/replace one).
func (s *Server) promptsPage(w http.ResponseWriter, r *http.Request) {
	var ps []store.Prompt
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListPrompts()
		ps = got
		return e
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><meta charset=utf-8><title>claw · prompts</title>`+
		`<style>body{font:14px system-ui;max-width:760px;margin:2rem auto;padding:0 1rem}`+
		`textarea{width:100%;height:8rem;font:13px monospace}label{font-weight:600}`+
		`form{margin:1.5rem 0;padding:1rem;border:1px solid #ddd;border-radius:8px}</style>`+
		`<h1>Agent prompts</h1><p>Edits apply on the agent's next run.</p>`)
	for _, p := range ps {
		fmt.Fprintf(w, `<form method=post action=/ui/prompts>`+
			`<label>%s / %s</label><input type=hidden name=namespace value=%q><input type=hidden name=name value=%q>`+
			`<textarea name=content>%s</textarea><button>Save</button> <small>updated %s</small></form>`,
			html.EscapeString(p.AgentNamespace), html.EscapeString(p.AgentName),
			p.AgentNamespace, p.AgentName, html.EscapeString(p.Content), html.EscapeString(p.UpdatedAt))
	}
	fmt.Fprint(w, `<form method=post action=/ui/prompts><label>New / replace</label>`+
		`<input name=namespace placeholder="namespace (default claw-agents)"> `+
		`<input name=name placeholder="agent name" required><br><br>`+
		`<textarea name=content placeholder="system prompt" required></textarea>`+
		`<button>Save</button></form>`)
}

func (s *Server) promptsSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeErr(w, http.StatusBadRequest, "bad form")
		return
	}
	ns := r.FormValue("namespace")
	if ns == "" {
		ns = "claw-agents"
	}
	name, content := r.FormValue("name"), r.FormValue("content")
	if name == "" || content == "" {
		writeErr(w, http.StatusBadRequest, "name and content are required")
		return
	}
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		return tx.SetPrompt(store.Prompt{AgentNamespace: ns, AgentName: name, Content: content})
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, "/ui/prompts", http.StatusSeeOther)
}
