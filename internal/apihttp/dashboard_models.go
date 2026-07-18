package apihttp

import (
	"net/http"
	"strings"

	"github.com/traego/kube-claw/internal/store"
)

// modelsPage: the LLM registry — add/edit models (Anthropic, OpenAI, or any
// OpenAI-compatible endpoint), pick the install default, delete. API keys are
// write-only: the form never echoes them, blank on edit keeps the stored key.
func (s *Server) modelsPage(w http.ResponseWriter, r *http.Request) {
	type row struct {
		store.Model
		HasKey bool
	}
	var rows []row
	if list, err := s.Models.List(r.Context()); err == nil {
		for _, m := range list {
			rows = append(rows, row{Model: m, HasKey: len(m.APIKeyCiphertext) > 0})
		}
	}
	body := `<p class=mut>The models agents can run on. The <b>default</b> serves every conversation
unless a thread switches ("use gpt5 for this") via the agent's <code>switch_model</code> tool —
only models registered here are reachable from chat. Provider <code>openai</code> speaks the
OpenAI-compatible wire format: set a base URL for self-hosted endpoints (vLLM, Ollama,
OpenRouter, …); leave the key blank for keyless local endpoints.</p>
<table><tr><th>Name</th><th>Provider</th><th>Model id</th><th>Endpoint</th><th>Key</th><th>Notes</th><th></th></tr>
{{range .D}}<tr>
<td><code>{{.Name}}</code>{{if .IsDefault}} <span class=pill Succeeded>default</span>{{end}}</td>
<td>{{.Provider}}</td><td><code>{{.ModelID}}</code></td>
<td class=mut>{{if .BaseURL}}{{.BaseURL}}{{else}}provider default{{end}}</td>
<td class=mut>{{if .HasKey}}set{{else}}—{{end}}</td>
<td class=mut>{{.Notes}}</td>
<td style="display:flex;gap:.4rem">
{{if not .IsDefault}}<form method=post action=/ui/models style=margin:0>
<input type=hidden name=action value=default><input type=hidden name=name value="{{.Name}}">
<button>Make default</button></form>{{end}}
<form method=post action=/ui/models style=margin:0 onsubmit="return confirm('Delete model {{.Name}}?')">
<input type=hidden name=action value=delete><input type=hidden name=name value="{{.Name}}">
<button style="background:#c5221f;border-color:#c5221f">Delete</button></form>
</td>
</tr>{{else}}<tr><td colspan=7 class=mut>No models registered — agents run on the legacy env config
(ANTHROPIC_API_KEY / CLAW_MODEL) until one is added here.</td></tr>{{end}}</table>
<h2>Add or update a model</h2>
<form method=post action=/ui/models>
<input type=hidden name=action value=upsert>
<label>Name <input name=name placeholder="gpt5" required> <span class=mut>the handle users type in chat — no spaces</span></label><br>
<label>Provider <select name=provider><option value=anthropic>anthropic</option><option value=openai>openai (or compatible)</option></select></label><br>
<label>Model id <input name=modelId placeholder="gpt-5.2" required></label><br>
<label>Base URL <input name=baseUrl placeholder="https://my-vllm.internal/v1"> <span class=mut>blank = provider default; self-hosted endpoints go here</span></label><br>
<label>API key <input name=apiKey type=password placeholder="write-only"> <span class=mut>blank keeps the existing key (or keyless)</span></label><br>
<label>Notes <input name=notes placeholder="when to use this model — shown to agents when listing"></label><br>
<label><input type=checkbox name=default value=1> make this the default</label><br>
<button type=submit>Save model</button>
</form>`
	s.renderDash(w, "models", "Models", body, rows)
}

func (s *Server) modelsSubmit(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	switch r.FormValue("action") {
	case "default":
		_ = s.Models.SetDefault(r.Context(), r.FormValue("name"))
	case "delete":
		_ = s.Models.Delete(r.Context(), r.FormValue("name"))
	case "upsert":
		m := store.Model{
			Name:     r.FormValue("name"),
			Provider: r.FormValue("provider"),
			ModelID:  r.FormValue("modelId"),
			BaseURL:  r.FormValue("baseUrl"),
			Notes:    r.FormValue("notes"),
		}
		if err := s.Models.Upsert(r.Context(), m, r.FormValue("apiKey")); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if list, err := s.Models.List(r.Context()); err == nil && (r.FormValue("default") == "1" || len(list) == 1) {
			_ = s.Models.SetDefault(r.Context(), strings.TrimSpace(r.FormValue("name")))
		}
	}
	http.Redirect(w, r, "/ui/models", http.StatusSeeOther)
}
