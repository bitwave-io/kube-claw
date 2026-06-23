package apihttp

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/store"
)

// The self-hosted admin dashboard (DESIGN.md §UI): secrets (rotate, never view),
// recent conversations for audit, agents, base images, and channel routing.
// Server-rendered html/template — no build step, no JS framework.

const dashHead = `<!doctype html><html><head><meta charset=utf-8>
<title>kube-claw · {{.Title}}</title>
<style>
:root{--ink:#1a1a1a;--mut:#666;--line:#e3e3e3;--accent:#2b6cb0;--bg:#fafafa}
*{box-sizing:border-box}body{font:14px/1.5 system-ui,sans-serif;margin:0;color:var(--ink);background:var(--bg)}
nav{display:flex;gap:.25rem;align-items:center;padding:.6rem 1rem;background:#fff;border-bottom:1px solid var(--line);position:sticky;top:0}
nav .brand{font-weight:700;margin-right:1rem}nav a{padding:.35rem .7rem;border-radius:6px;color:var(--ink);text-decoration:none}
nav a:hover{background:#f0f0f0}nav a.on{background:var(--accent);color:#fff}
main{max-width:1000px;margin:1.5rem auto;padding:0 1rem}
h1{font-size:1.3rem;margin:.2rem 0 1rem}h2{font-size:1rem;margin:1.5rem 0 .5rem}
table{width:100%;border-collapse:collapse;background:#fff;border:1px solid var(--line);border-radius:8px;overflow:hidden}
th,td{text-align:left;padding:.55rem .7rem;border-bottom:1px solid var(--line);vertical-align:top}
th{background:#f7f7f7;font-size:.8rem;text-transform:uppercase;letter-spacing:.03em;color:var(--mut)}
tr:last-child td{border-bottom:0}code{background:#f0f0f0;padding:.1rem .35rem;border-radius:4px;font-size:.85em}
.pill{display:inline-block;padding:.1rem .5rem;border-radius:999px;font-size:.78rem;font-weight:600}
.Succeeded{background:#e6f4ea;color:#137333}.Failed{background:#fce8e6;color:#c5221f}
.Blocked{background:#fef7e0;color:#b06000}.Running,.Pending{background:#e8f0fe;color:#1967d2}
.mut{color:var(--mut)}.cards{display:flex;gap:1rem;flex-wrap:wrap;margin:1rem 0}
.card{flex:1;min-width:140px;background:#fff;border:1px solid var(--line);border-radius:8px;padding:1rem}
.card .n{font-size:1.8rem;font-weight:700}.card a{color:var(--accent);text-decoration:none}
button{font:inherit;padding:.3rem .7rem;border:1px solid var(--accent);background:var(--accent);color:#fff;border-radius:6px;cursor:pointer}
.snip{max-width:340px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;display:block}
.link{word-break:break-all;background:#f0f0f0;padding:.6rem;border-radius:6px;display:block}
</style></head><body>
<nav><span class=brand>🦞 kube-claw</span>
<a href=/ui/dashboard class="{{if eq .Active "dashboard"}}on{{end}}">Dashboard</a>
<a href=/ui/secrets class="{{if eq .Active "secrets"}}on{{end}}">Secrets</a>
<a href=/ui/conversations class="{{if eq .Active "conversations"}}on{{end}}">Conversations</a>
<a href=/ui/agents class="{{if eq .Active "agents"}}on{{end}}">Agents</a>
<a href=/ui/base-images class="{{if eq .Active "images"}}on{{end}}">Images</a>
<a href=/ui/prompts class="{{if eq .Active "prompts"}}on{{end}}">Prompts</a>
<a href=/ui/channels class="{{if eq .Active "channels"}}on{{end}}">Channels</a>
</nav><main><h1>{{.Title}}</h1>`

const dashFoot = `</main></body></html>`

func (s *Server) renderDash(w http.ResponseWriter, active, title, body string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t := template.Must(template.New("p").Parse(dashHead + body + dashFoot))
	type wrap struct {
		Title, Active string
		D             any
	}
	_ = t.Execute(w, wrap{Title: title, Active: active, D: data})
}

func (s *Server) dashboardHome(w http.ResponseWriter, r *http.Request) {
	var secrets, runs, images int
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if ss, e := tx.ListSecrets(); e == nil {
			secrets = len(ss)
		}
		if rs, e := tx.ListRuns(1000); e == nil {
			runs = len(rs)
		}
		if im, e := tx.ListBaseImages(); e == nil {
			images = len(im)
		}
		return nil
	})
	body := `<p class=mut>Self-hosted control plane for sandboxed, Slack-triggered AI agents.</p>
<div class=cards>
<div class=card><div class=n>{{.D.Secrets}}</div><a href=/ui/secrets>Secrets</a></div>
<div class=card><div class=n>{{.D.Runs}}</div><a href=/ui/conversations>Conversations</a></div>
<div class=card><div class=n>{{.D.Images}}</div><a href=/ui/base-images>Base images</a></div>
</div>`
	s.renderDash(w, "dashboard", "Dashboard", body, map[string]int{"Secrets": secrets, "Runs": runs, "Images": images})
}

func (s *Server) secretsPage(w http.ResponseWriter, r *http.Request) {
	var secs []store.Secret
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListSecrets()
		secs = got
		return e
	})
	body := `<p class=mut>Values are write-only — you can rotate a secret (provide a new value via a one-time link) but never view it.</p>
<table><tr><th>Name</th><th>Namespace</th><th>Type</th><th>Description</th><th>Approvers</th><th>Created</th><th></th></tr>
{{range .D}}<tr>
<td><code>{{.Name}}</code></td><td>{{.Namespace}}</td><td>{{.Type}}</td>
<td class=mut>{{.Description}}</td>
<td>{{range .Granters}}<code>{{.}}</code> {{end}}</td>
<td class=mut>{{.CreatedAt}}</td>
<td><form method=post action=/ui/secrets/rotate style=margin:0>
<input type=hidden name=namespace value="{{.Namespace}}"><input type=hidden name=name value="{{.Name}}">
<button>Rotate</button></form></td>
</tr>{{else}}<tr><td colspan=7 class=mut>No secrets yet.</td></tr>{{end}}</table>`
	s.renderDash(w, "secrets", "Secrets", body, secs)
}

// rotateSecret mints a one-time intake link to provide a NEW value (rotation).
// It never reveals the current value.
func (s *Server) rotateSecret(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ns, name := r.FormValue("namespace"), r.FormValue("name")
	if ns == "" || name == "" {
		writeErr(w, http.StatusBadRequest, "namespace and name required")
		return
	}
	tok, err := s.Secrets.MintIntakeToken(r.Context(), ns, name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	link := fmt.Sprintf("%s/ui/secret-intake/%s", s.UIBase, tok)
	body := `<p>One-time link to set a new value for <code>{{.D.Name}}</code> — open it and submit the new secret. It expires after use.</p>
<p><a class=link href="{{.D.Link}}">{{.D.Link}}</a></p>
<p><a href=/ui/secrets>&larr; back to secrets</a></p>`
	s.renderDash(w, "secrets", "Rotate "+name, body, map[string]string{"Name": name, "Link": link})
}

func (s *Server) conversationsPage(w http.ResponseWriter, r *http.Request) {
	type turn struct {
		store.Run
		Channel, Input, Output string
	}
	var turns []turn
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		runs, e := tx.ListRuns(50)
		if e != nil {
			return e
		}
		for _, run := range runs {
			t := turn{Run: run, Channel: jsonField(run.Source, "channel"), Input: jsonField(run.Input, "text")}
			if outs, e := tx.ListOutputs(run.ID); e == nil && len(outs) > 0 {
				t.Output = outs[len(outs)-1].Content
			}
			turns = append(turns, t)
		}
		return nil
	})
	body := `<p class=mut>Most recent 50 runs — for audit. Inputs and answers are shown; secret values never appear here.</p>
<table><tr><th>When</th><th>Agent</th><th>Channel</th><th>Phase</th><th>Request</th><th>Answer</th></tr>
{{range .D}}<tr>
<td class=mut>{{.CreatedAt}}</td><td><code>{{.AgentName}}</code></td><td class=mut>{{.Channel}}</td>
<td><span class="pill {{.Phase}}">{{.Phase}}</span></td>
<td><span class=snip>{{.Input}}</span></td><td><span class=snip>{{.Output}}</span></td>
</tr>{{else}}<tr><td colspan=6 class=mut>No conversations yet.</td></tr>{{end}}</table>`
	s.renderDash(w, "conversations", "Conversations", body, turns)
}

func (s *Server) agentsPage(w http.ResponseWriter, r *http.Request) {
	var list clawv1alpha1.AgentList
	_ = s.Reader.List(r.Context(), &list, client.InNamespace("claw-agents"))
	body := `<p class=mut>Idle timeout is how long a session pod stays warm for follow-ups before scaling to zero (reset on each turn). Edit it inline.</p>
<table><tr><th>Name</th><th>Namespace</th><th>Base image</th><th>Phase</th><th>Idle (warm) timeout</th><th>Secrets</th></tr>
{{range .D.Items}}<tr>
<td><code>{{.Name}}</code></td><td>{{.Namespace}}</td><td>{{.Spec.BaseImageRef}}</td>
<td><span class="pill {{.Status.Phase}}">{{.Status.Phase}}</span></td>
<td><form method=post action=/ui/agents/idle style="margin:0;display:flex;gap:.3rem">
<input type=hidden name=namespace value="{{.Namespace}}"><input type=hidden name=name value="{{.Name}}">
<input name=idle value="{{.Spec.Runtime.IdleTimeout}}" size=6 placeholder=15m><button>Set</button></form></td>
<td>{{range .Spec.Secrets}}<code>{{.Name}}</code> {{end}}</td>
</tr>{{else}}<tr><td colspan=6 class=mut>No agents.</td></tr>{{end}}</table>`
	s.renderDash(w, "agents", "Agents", body, &list)
}

// agentSetIdle updates an agent's warm-session idle timeout from the UI.
func (s *Server) agentSetIdle(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ns, name, idle := r.FormValue("namespace"), r.FormValue("name"), r.FormValue("idle")
	if ns == "" || name == "" || idle == "" {
		writeErr(w, http.StatusBadRequest, "namespace, name, idle required")
		return
	}
	var agent clawv1alpha1.Agent
	if err := s.K8s.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &agent); err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}
	agent.Spec.Runtime.IdleTimeout = idle
	if err := s.K8s.Update(r.Context(), &agent); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, "/ui/agents", http.StatusSeeOther)
}

func (s *Server) channelsPage(w http.ResponseWriter, r *http.Request) {
	var cfgs []store.ChannelConfig
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListChannelConfigs()
		cfgs = got
		return e
	})
	body := `<p class=mut>Channels configure themselves when the bot is added (it DMs the inviter). This is the resulting routing.</p>
<table><tr><th>Channel</th><th>Agent</th><th>Responds to</th><th>Replies</th><th>Updated</th></tr>
{{range .D}}<tr>
<td><code>{{.Channel}}</code></td><td><code>{{.AgentName}}</code></td>
<td>{{if .MentionRequired}}@mentions only{{else}}every message{{end}}</td>
<td>{{if .ThreadOnly}}in threads{{else}}in channel{{end}}</td>
<td class=mut>{{.UpdatedAt}}</td>
</tr>{{else}}<tr><td colspan=5 class=mut>No channels configured yet — add the bot to a channel.</td></tr>{{end}}</table>`
	s.renderDash(w, "channels", "Channels", body, cfgs)
}

// jsonField pulls a string field out of an opaque JSON blob (run Source/Input).
func jsonField(blob, key string) string {
	m := map[string]any{}
	if json.Unmarshal([]byte(blob), &m) != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
