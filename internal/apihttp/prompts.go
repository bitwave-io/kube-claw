package apihttp

import (
	"encoding/json"
	"net/http"

	slackrouter "github.com/traego/kube-claw/internal/router/slack"
	"github.com/traego/kube-claw/internal/store"
)

// claimNextTurn lets a warm session pod claim the next pending turn (Slack
// follow-up message) in its thread. 204 when there's nothing pending.
func (s *Server) claimNextTurn(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	pod := r.URL.Query().Get("pod")
	var run store.Run
	var ok bool
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, claimed, e := tx.ClaimNextPendingTurn(sid, pod)
		run, ok = got, claimed
		return e
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(run.Input), &in)
	writeJSON(w, http.StatusOK, map[string]string{"runId": run.ID, "input": in.Text})
}

// sessionSleep is called by a warm pod when it idles out — it adds a 💤 to the
// thread's top-level message (the session id IS that message's ts) so the channel
// sees the agent went to sleep.
func (s *Server) sessionSleep(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	if s.Notifier != nil {
		var channel string
		_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
			runs, e := tx.ListRunsBySession(sid, 1)
			if e == nil && len(runs) > 0 {
				channel = slackrouter.SlackChannel(runs[0].Source)
			}
			return e
		})
		if channel != "" {
			_ = s.Notifier.AddReaction(r.Context(), channel, sid, "zzz")
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

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
	body := `<p class=mut>Per-agent system prompts (DB overrides of the agent's CRD prompt). Edits apply on the agent's next run.</p>
{{range .D}}<form method=post action=/ui/prompts style="background:#fff;border:1px solid var(--line);border-radius:8px;padding:1rem;margin:1rem 0">
<label><code>{{.AgentNamespace}}/{{.AgentName}}</code></label>
<input type=hidden name=namespace value="{{.AgentNamespace}}"><input type=hidden name=name value="{{.AgentName}}">
<textarea name=content style="width:100%;height:7rem;font:13px monospace">{{.Content}}</textarea><br>
<button>Save</button> <span class=mut>updated {{.UpdatedAt}}</span></form>
{{else}}<p class=mut>No stored prompt overrides — agents use the prompt from their CRD.</p>{{end}}

<h2>New / replace</h2>
<form method=post action=/ui/prompts style="background:#fff;border:1px solid var(--line);border-radius:8px;padding:1rem;max-width:660px">
<label>Namespace</label><br><input name=namespace placeholder="claw-agents" style="width:100%"><br><br>
<label>Agent name</label><br><input name=name required style="width:100%"><br><br>
<label>System prompt</label><br><textarea name=content required style="width:100%;height:7rem;font:13px monospace"></textarea><br><br>
<button>Save</button></form>`
	s.renderDash(w, "prompts", "Agent prompts", body, ps)
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
