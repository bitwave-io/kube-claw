package apihttp

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/traego/kube-claw/internal/store"
)

// modelsPage: the LLM registry — add/edit models (Anthropic, OpenAI, or any
// OpenAI-compatible endpoint), pick the install default, delete. API keys are
// write-only: the form never echoes them, blank on edit keeps the stored key.
func (s *Server) modelsPage(w http.ResponseWriter, r *http.Request) {
	type modelRow struct {
		store.Model
		HasKey bool
	}
	type provRow struct {
		store.Provider
		HasKey bool
	}
	var models []modelRow
	if list, err := s.Models.List(r.Context()); err == nil {
		for _, m := range list {
			models = append(models, modelRow{Model: m, HasKey: len(m.APIKeyCiphertext) > 0})
		}
	}
	var providers []provRow
	if list, err := s.Models.ListProviders(r.Context()); err == nil {
		for _, p := range list {
			providers = append(providers, provRow{Provider: p, HasKey: len(p.APIKeyCiphertext) > 0})
		}
	}
	data := struct {
		Models    []modelRow
		Providers []provRow
	}{models, providers}
	body := `<p class=mut>The models agents can run on. The <b>default</b> serves every conversation
unless a thread switches ("use gpt5 for this") via the agent's <code>switch_model</code> tool —
only <b>enabled</b> models are reachable from chat. Register a <b>provider</b> to pull its whole
catalog automatically (refreshed periodically); disable the individual models you don't want.
Add a model by hand for local/self-hosted endpoints.</p>

<h2>Providers</h2>
<p class=mut>A hosted API whose model list is pulled on a schedule. Keys are AEAD-encrypted and
write-only. <code>openai</code> and <code>gemini</code> models run over the OpenAI-compatible wire
format; set a base URL only for a gateway or self-hosted proxy.</p>
<table><tr><th>Name</th><th>Kind</th><th>Endpoint</th><th>Key</th><th>Prefix</th><th>Last synced</th><th>Status</th><th></th></tr>
{{range .D.Providers}}<tr>
<td><code>{{.Name}}</code>{{if not .Enabled}} <span class=mut>(disabled)</span>{{end}}</td>
<td>{{.Kind}}</td>
<td class=mut>{{if .BaseURL}}{{.BaseURL}}{{else}}provider default{{end}}</td>
<td class=mut>{{if .HasKey}}set{{else}}—{{end}}</td>
<td class=mut>{{if .ModelPrefix}}{{.ModelPrefix}}{{else}}—{{end}}</td>
<td class=mut>{{if .LastSyncedAt}}{{.LastSyncedAt}}{{else}}never{{end}}</td>
<td class=mut>{{if .LastSyncError}}<span style="color:#c5221f">{{.LastSyncError}}</span>{{else}}ok{{end}}</td>
<td style="display:flex;gap:.4rem">
<form method=post action=/ui/models style=margin:0>
<input type=hidden name=action value=provider-sync><input type=hidden name=name value="{{.Name}}">
<button>Refresh now</button></form>
<form method=post action=/ui/models style=margin:0 onsubmit="return confirm('Delete provider {{.Name}} and all its models?')">
<input type=hidden name=action value=provider-delete><input type=hidden name=name value="{{.Name}}">
<button style="background:#c5221f;border-color:#c5221f">Delete</button></form>
</td>
</tr>{{else}}<tr><td colspan=8 class=mut>No providers registered.</td></tr>{{end}}</table>
<h3>Add or update a provider</h3>
<form method=post action=/ui/models>
<input type=hidden name=action value=provider-upsert>
<label>Name <input name=name placeholder="openai-prod" required> <span class=mut>a handle for this connection — no spaces</span></label><br>
<label>Kind <select name=kind><option value=openai>openai</option><option value=anthropic>anthropic</option><option value=gemini>gemini</option></select></label><br>
<label>Base URL <input name=baseUrl placeholder="blank = provider default"> <span class=mut>only for a gateway / self-hosted proxy</span></label><br>
<label>API key <input name=apiKey type=password placeholder="write-only"> <span class=mut>blank keeps the existing key</span></label><br>
<label>Model prefix <input name=modelPrefix placeholder="optional, e.g. oai-"> <span class=mut>prepended to discovered handles to avoid clashes</span></label><br>
<label><input type=checkbox name=enabled value=1 checked> enabled (sync this provider)</label><br>
<button type=submit>Save provider</button>
</form>

<h2>Models</h2>
<table><tr><th>Name</th><th>Enabled</th><th>Source</th><th>Provider</th><th>Model id</th><th>Endpoint</th><th>Key</th><th>Max out</th><th>Notes</th><th></th></tr>
{{range .D.Models}}<tr>
<td><code>{{.Name}}</code>{{if .IsDefault}} <span class=pill Succeeded>default</span>{{end}}</td>
<td>{{if .Enabled}}yes{{else}}<span class=mut>no</span>{{end}}</td>
<td class=mut>{{if .ProviderName}}{{.ProviderName}}{{else}}manual{{end}}</td>
<td>{{.Provider}}</td><td><code>{{.ModelID}}</code></td>
<td class=mut>{{if .BaseURL}}{{.BaseURL}}{{else}}provider default{{end}}</td>
<td class=mut>{{if .HasKey}}set{{else if .ProviderName}}via provider{{else}}—{{end}}</td>
<td class=mut>{{if .MaxTokens}}{{.MaxTokens}}{{else}}—{{end}}</td>
<td class=mut>{{.Notes}}</td>
<td style="display:flex;gap:.4rem">
{{if not .IsDefault}}<form method=post action=/ui/models style=margin:0>
<input type=hidden name=action value=default><input type=hidden name=name value="{{.Name}}">
<button>Make default</button></form>{{end}}
<form method=post action=/ui/models style=margin:0>
<input type=hidden name=action value=enabled><input type=hidden name=name value="{{.Name}}">
<input type=hidden name=enabled value="{{if .Enabled}}0{{else}}1{{end}}">
<button>{{if .Enabled}}Disable{{else}}Enable{{end}}</button></form>
<form method=post action=/ui/models style=margin:0 onsubmit="return confirm('Delete model {{.Name}}?')">
<input type=hidden name=action value=delete><input type=hidden name=name value="{{.Name}}">
<button style="background:#c5221f;border-color:#c5221f">Delete</button></form>
</td>
</tr>{{else}}<tr><td colspan=10 class=mut>No models registered — agents run on the legacy env config
(ANTHROPIC_API_KEY / CLAW_MODEL) until a provider is synced or a model is added here.</td></tr>{{end}}</table>
<h3>Add or update a model (manual / local)</h3>
<form method=post action=/ui/models>
<input type=hidden name=action value=upsert>
<label>Name <input name=name placeholder="local-llama" required> <span class=mut>the handle users type in chat — no spaces</span></label><br>
<label>Provider <select name=provider><option value=anthropic>anthropic</option><option value=openai>openai (or compatible)</option></select></label><br>
<label>Model id <input name=modelId placeholder="llama-3.3-70b" required></label><br>
<label>Base URL <input name=baseUrl placeholder="https://my-vllm.internal/v1"> <span class=mut>blank = provider default; self-hosted endpoints go here</span></label><br>
<label>API key <input name=apiKey type=password placeholder="write-only"> <span class=mut>blank keeps the existing key (or keyless)</span></label><br>
<label>Max output tokens <input name=maxTokens placeholder="8192"> <span class=mut>blank = provider default; set when the model's context is smaller than 32k</span></label><br>
<label>Notes <input name=notes placeholder="when to use this model — shown to agents when listing"></label><br>
<label><input type=checkbox name=default value=1> make this the default</label><br>
<button type=submit>Save model</button>
</form>`
	s.renderDash(w, "models", "Models", body, data)
}

func (s *Server) modelsSubmit(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	switch r.FormValue("action") {
	case "default":
		if err := s.Models.SetDefault(r.Context(), r.FormValue("name")); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	case "delete":
		if err := s.Models.Delete(r.Context(), r.FormValue("name")); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	case "enabled":
		if err := s.Models.SetModelEnabled(r.Context(), r.FormValue("name"), r.FormValue("enabled") == "1"); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	case "provider-upsert":
		p := store.Provider{
			Name:        r.FormValue("name"),
			Kind:        r.FormValue("kind"),
			BaseURL:     r.FormValue("baseUrl"),
			ModelPrefix: r.FormValue("modelPrefix"),
			Enabled:     r.FormValue("enabled") == "1",
		}
		if err := s.Models.UpsertProvider(r.Context(), p, r.FormValue("apiKey")); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	case "provider-delete":
		if err := s.Models.DeleteProvider(r.Context(), r.FormValue("name")); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	case "provider-sync":
		if err := s.Models.SyncProvider(r.Context(), r.FormValue("name")); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	case "upsert":
		maxTokens := 0
		if v := strings.TrimSpace(r.FormValue("maxTokens")); v != "" {
			var err error
			if maxTokens, err = strconv.Atoi(v); err != nil {
				writeErr(w, http.StatusBadRequest, "max output tokens must be a number")
				return
			}
		}
		m := store.Model{
			Name:      r.FormValue("name"),
			Provider:  r.FormValue("provider"),
			ModelID:   r.FormValue("modelId"),
			BaseURL:   r.FormValue("baseUrl"),
			MaxTokens: maxTokens,
			Notes:     r.FormValue("notes"),
		}
		if err := s.Models.Upsert(r.Context(), m, r.FormValue("apiKey"), r.FormValue("default") == "1"); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	http.Redirect(w, r, "/ui/models", http.StatusSeeOther)
}
