package apihttp

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
.conv{background:#fff;border:1px solid var(--line);border-radius:8px;margin:1rem 0;overflow:hidden}
.conv h3{margin:0;padding:.5rem .8rem;background:#f7f7f7;font-size:.85rem;font-weight:600;border-bottom:1px solid var(--line);display:flex;justify-content:space-between;gap:1rem}
.turn{padding:.55rem .8rem;border-bottom:1px solid #f1f1f1;white-space:pre-wrap;word-break:break-word}.turn:last-child{border:0}
.turn .who{font-weight:700;font-size:.72rem;text-transform:uppercase;letter-spacing:.03em;display:block;margin-bottom:.15rem}
.turn.u{background:#fbfdff}.turn.u .who{color:#1967d2}.turn.a .who{color:#137333}
</style></head><body>
<nav><span class=brand>🦞 kube-claw</span>
<a href=/ui/dashboard class="{{if eq .Active "dashboard"}}on{{end}}">Dashboard</a>
<a href=/ui/secrets class="{{if eq .Active "secrets"}}on{{end}}">Secrets</a>
<a href=/ui/gitrepos class="{{if eq .Active "gitrepos"}}on{{end}}">Git repos</a>
<a href=/ui/requests class="{{if eq .Active "requests"}}on{{end}}">Requests</a>
<a href=/ui/conversations class="{{if eq .Active "conversations"}}on{{end}}">Conversations</a>
<a href=/ui/audit class="{{if eq .Active "audit"}}on{{end}}">Audit</a>
<a href=/ui/agents class="{{if eq .Active "agents"}}on{{end}}">Agents</a>
<a href=/ui/schedules class="{{if eq .Active "schedules"}}on{{end}}">Schedules</a>
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
<td style="display:flex;gap:.4rem">
<form method=post action=/ui/secrets/rotate style=margin:0>
<input type=hidden name=namespace value="{{.Namespace}}"><input type=hidden name=name value="{{.Name}}">
<button>Rotate</button></form>
<form method=post action=/ui/secrets/delete style=margin:0 onsubmit="return confirm('Delete secret {{.Name}}? This removes its value, grants, and history and cannot be undone.')">
<input type=hidden name=namespace value="{{.Namespace}}"><input type=hidden name=name value="{{.Name}}">
<button style="background:#c5221f;border-color:#c5221f">Delete</button></form>
</td>
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
	tok, err := s.Secrets.MintIntakeToken(r.Context(), ns, name, "")
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

// deleteSecret removes a secret (and its versions/grants) from the UI.
func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ns, name := r.FormValue("namespace"), r.FormValue("name")
	if ns == "" || name == "" {
		writeErr(w, http.StatusBadRequest, "namespace and name required")
		return
	}
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if e := tx.DeleteSecret(ns, name); e != nil {
			return e
		}
		return tx.AppendAudit(store.AuditEvent{Type: "secret.deleted", Actor: "ui",
			Detail: map[string]any{"namespace": ns, "secret": name}})
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, "/ui/secrets", http.StatusSeeOther)
}

// requestsPage lists pending on-demand access requests with the agent's reason
// ("why") and who it's for, plus Approve/Deny (break-glass: the authenticated UI
// operator is trusted, so it bypasses the Slack granter check).
func (s *Server) requestsPage(w http.ResponseWriter, r *http.Request) {
	type reqRow struct {
		store.SecretRequest
		SecretName string
	}
	var rows []reqRow
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		reqs, e := tx.ListSecretRequests("Pending")
		if e != nil {
			return e
		}
		for _, rq := range reqs {
			name := rq.SecretName
			if name == "" {
				if sec, e := tx.GetSecret(rq.AgentNamespace, rq.SecretName); e == nil {
					name = sec.Name
				}
			}
			rows = append(rows, reqRow{SecretRequest: rq, SecretName: name})
		}
		return nil
	})
	// Pending git-repo access requests share this page (same break-glass flow).
	var gitRows []store.GitRepoRequest
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListGitRepoRequests("Pending")
		gitRows = got
		return e
	})
	data := struct {
		Secrets  []reqRow
		GitRepos []store.GitRepoRequest
	}{rows, gitRows}
	body := `<p class=mut>Pending access requests. The agent's reason and who it's for are shown so you can make an informed call. Approving here is break-glass (it bypasses the Slack granter check) and is audited.</p>
<h2>Secrets</h2>
<table><tr><th>When</th><th>Agent</th><th>Secret</th><th>For (who)</th><th>Reason (why)</th><th></th></tr>
{{range .D.Secrets}}<tr>
<td class=mut>{{.CreatedAt}}</td><td><code>{{.AgentName}}</code></td><td><code>{{.SecretName}}</code></td>
<td>{{if .RequestedBy}}{{.RequestedBy}}{{else}}<span class=mut>—</span>{{end}}</td>
<td>{{if .Context}}{{.Context}}{{else}}<span class=mut>(none)</span>{{end}}</td>
<td style="display:flex;gap:.4rem">
<form method=post action=/ui/requests/approve style=margin:0><input type=hidden name=id value="{{.ID}}"><button>Approve</button></form>
<form method=post action=/ui/requests/deny style=margin:0><input type=hidden name=id value="{{.ID}}"><button style="background:#c5221f;border-color:#c5221f">Deny</button></form>
</td>
</tr>{{else}}<tr><td colspan=6 class=mut>No pending secret requests.</td></tr>{{end}}</table>
<h2>Git repos</h2>
<table><tr><th>When</th><th>Agent</th><th>Repo</th><th>Access</th><th>For (who)</th><th>Reason (why)</th><th></th></tr>
{{range .D.GitRepos}}<tr>
<td class=mut>{{.CreatedAt}}</td><td><code>{{.AgentName}}</code></td><td><code>{{.RepoName}}</code></td>
<td><code>{{.Access}}</code></td>
<td>{{if .RequestedBy}}{{.RequestedBy}}{{else}}<span class=mut>—</span>{{end}}</td>
<td>{{if .Context}}{{.Context}}{{else}}<span class=mut>(none)</span>{{end}}</td>
<td style="display:flex;gap:.4rem">
<form method=post action=/ui/gitrepo-requests/approve style=margin:0><input type=hidden name=id value="{{.ID}}"><button>Approve</button></form>
<form method=post action=/ui/gitrepo-requests/deny style=margin:0><input type=hidden name=id value="{{.ID}}"><button style="background:#c5221f;border-color:#c5221f">Deny</button></form>
</td>
</tr>{{else}}<tr><td colspan=7 class=mut>No pending git-repo requests.</td></tr>{{end}}</table>`
	s.renderDash(w, "requests", "Access requests", body, data)
}

// uiApproveRequest approves a pending request from the dashboard (break-glass).
func (s *Server) uiApproveRequest(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id := r.FormValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id required")
		return
	}
	if _, err := s.Approvals.Approve(r.Context(), id, "ui", "approved via dashboard"); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, "/ui/requests", http.StatusSeeOther)
}

// uiDenyRequest denies a pending request from the dashboard.
func (s *Server) uiDenyRequest(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id := r.FormValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id required")
		return
	}
	if err := s.Approvals.Deny(r.Context(), id, "ui", "denied via dashboard"); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, "/ui/requests", http.StatusSeeOther)
}

// auditPage shows the recent hash-chained audit log (value-free).
func (s *Server) auditPage(w http.ResponseWriter, r *http.Request) {
	var rows []store.AuditRecord
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListAudit(200)
		rows = got
		return e
	})
	body := `<p class=mut>The most recent 200 audit events — append-only and hash-chained (tamper-evident). Secret values never appear here.</p>
<table><tr><th>When</th><th>Event</th><th>Actor</th><th>Detail</th></tr>
{{range .D}}<tr>
<td class=mut>{{.TS}}</td><td><code>{{.Type}}</code></td><td>{{.Actor}}</td>
<td class=mut><span class=snip>{{.Detail}}</span></td>
</tr>{{else}}<tr><td colspan=4 class=mut>No audit events yet.</td></tr>{{end}}</table>`
	s.renderDash(w, "audit", "Audit log", body, rows)
}

func (s *Server) conversationsPage(w http.ResponseWriter, r *http.Request) {
	type turn struct{ When, Input, Output, Phase string }
	type conv struct {
		Channel, Agent, When string
		Turns                []turn
	}
	var order []string
	byKey := map[string]*conv{}
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		runs, e := tx.ListRuns(300) // newest-first; grouped into threads below
		if e != nil {
			return e
		}
		for _, run := range runs {
			// A conversation = a Slack thread (SessionID). Non-session (CLI) runs
			// stand alone, keyed by their own id.
			key := run.SessionID
			if key == "" {
				key = run.ID
			}
			c, ok := byKey[key]
			if !ok {
				c = &conv{Channel: jsonField(run.Source, "channel"), Agent: run.AgentName, When: run.CreatedAt}
				byKey[key] = c
				order = append(order, key) // first-seen = most recent activity
			}
			out := ""
			if outs, e := tx.ListOutputs(run.ID); e == nil && len(outs) > 0 {
				out = outs[len(outs)-1].Content
			}
			t := turn{When: run.CreatedAt, Input: jsonField(run.Input, "text"), Output: out, Phase: run.Phase}
			c.Turns = append([]turn{t}, c.Turns...) // prepend → chronological within the thread
		}
		return nil
	})
	convs := make([]*conv, 0, len(order))
	for _, k := range order {
		convs = append(convs, byKey[k])
	}
	body := `<p class=mut>Each block is one continuous conversation (a Slack thread); turns are in order. Secret values never appear here.</p>
{{range .D}}<div class=conv>
<h3><span>{{if .Channel}}#{{.Channel}}{{else}}(direct){{end}} · {{.Agent}}</span><span class=mut>{{.When}}</span></h3>
{{range .Turns}}
<div class="turn u"><span class=who>user</span>{{.Input}}</div>
<div class="turn a"><span class=who>kube-claw</span>{{if .Output}}{{.Output}}{{else}}<span class="pill {{.Phase}}">{{.Phase}}</span>{{end}}</div>
{{end}}
</div>{{else}}<p class=mut>No conversations yet.</p>{{end}}`
	s.renderDash(w, "conversations", "Conversations", body, convs)
}

func (s *Server) agentsPage(w http.ResponseWriter, r *http.Request) {
	var list clawv1alpha1.AgentList
	_ = s.Reader.List(r.Context(), &list, client.InNamespace("claw-agents"))
	var imgs []store.BaseImage
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListBaseImages()
		imgs = got
		return e
	})
	body := `<p class=mut>Agents pair a prompt with an image. The router picks the best-fit agent per request (by its prompt). Idle timeout is how long the pod stays warm for follow-ups (editable inline).</p>
<table><tr><th>Name</th><th>Base image</th><th>Phase</th><th>Idle (warm) timeout</th><th>Secrets</th><th>Prompt</th></tr>
{{range .D.Agents.Items}}<tr>
<td><code>{{.Name}}</code><br><a class=mut href="/ui/agents/edit?ns={{.Namespace}}&name={{.Name}}">edit</a></td><td>{{.Spec.BaseImageRef}}</td>
<td><span class="pill {{.Status.Phase}}">{{.Status.Phase}}</span></td>
<td><form method=post action=/ui/agents/idle style="margin:0;display:flex;gap:.3rem">
<input type=hidden name=namespace value="{{.Namespace}}"><input type=hidden name=name value="{{.Name}}">
<input name=idle value="{{.Spec.Runtime.IdleTimeout}}" size=6 placeholder=15m><button>Set</button></form></td>
<td>{{range .Spec.Secrets}}<code>{{.Name}}</code> {{end}}</td>
<td class=mut><span class=snip>{{if .Spec.Model}}{{.Spec.Model.SystemPrompt}}{{end}}</span></td>
</tr>{{else}}<tr><td colspan=6 class=mut>No agents yet.</td></tr>{{end}}</table>

<h2>Create an agent</h2>
<form method=post action=/ui/agents/create style="background:#fff;border:1px solid var(--line);border-radius:8px;padding:1rem;max-width:640px">
<label>Name</label><br><input name=name placeholder="e.g. gcp-cost" required style="width:100%"><br><br>
<label>Image</label><br>
<select name=baseImageRef required style="width:100%"><option value="">— select a base image —</option>
{{range .D.Images}}<option value="{{.Name}}">{{.Name}} — {{.Description}}</option>{{end}}</select><br><br>
<label>Prompt (what this agent is for + how it should behave — this also drives routing)</label><br>
<textarea name=prompt required style="width:100%;height:6rem;font:13px monospace" placeholder="You are a GCP cost assistant. Use gcloud/bq to answer billing and spend questions..."></textarea><br><br>
<button>Create agent</button>
</form>`
	s.renderDash(w, "agents", "Agents", body, map[string]any{"Agents": &list, "Images": imgs})
}

// uiCreateAgent creates an Agent CRD from the dashboard form (image from the
// dropdown, prompt as the system prompt + routing description).
func (s *Server) uiCreateAgent(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ns := r.FormValue("namespace")
	if ns == "" {
		ns = "claw-agents"
	}
	name, base, prompt := r.FormValue("name"), r.FormValue("baseImageRef"), r.FormValue("prompt")
	if name == "" || base == "" {
		writeErr(w, http.StatusBadRequest, "name and image are required")
		return
	}
	agent := &clawv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: clawv1alpha1.AgentSpec{
			BaseImageRef: base,
			Runtime:      clawv1alpha1.RuntimeSpec{Mode: "scaleToZeroSession", IdleTimeout: "15m"},
		},
	}
	if prompt != "" {
		agent.Spec.Model = &clawv1alpha1.ModelSpec{SystemPrompt: prompt}
	}
	if err := s.K8s.Create(r.Context(), agent); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, "/ui/agents", http.StatusSeeOther)
}

// agentEditPage renders an edit form for one agent (image dropdown pre-selected,
// prompt pre-filled).
func (s *Server) agentEditPage(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("ns")
	if ns == "" {
		ns = "claw-agents"
	}
	name := r.URL.Query().Get("name")
	var agent clawv1alpha1.Agent
	if err := s.Reader.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &agent); err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}
	var imgs []store.BaseImage
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListBaseImages()
		imgs = got
		return e
	})
	prompt := ""
	if agent.Spec.Model != nil {
		prompt = agent.Spec.Model.SystemPrompt
	}
	body := `<form method=post action=/ui/agents/update style="background:#fff;border:1px solid var(--line);border-radius:8px;padding:1rem;max-width:640px">
<input type=hidden name=namespace value="{{.D.NS}}"><input type=hidden name=name value="{{.D.Name}}">
<p>Editing <code>{{.D.Name}}</code></p>
<label>Image</label><br><select name=baseImageRef style="width:100%">
{{range .D.Images}}<option value="{{.Name}}" {{if eq .Name $.D.Current}}selected{{end}}>{{.Name}} — {{.Description}}</option>{{end}}</select><br><br>
<label>Prompt</label><br><textarea name=prompt style="width:100%;height:8rem;font:13px monospace">{{.D.Prompt}}</textarea><br><br>
<button>Save</button> <a href=/ui/agents style="margin-left:.5rem">cancel</a>
</form>`
	s.renderDash(w, "agents", "Edit "+name, body,
		map[string]any{"NS": ns, "Name": name, "Prompt": prompt, "Current": agent.Spec.BaseImageRef, "Images": imgs})
}

// agentUpdate applies an edit to an agent's image + prompt.
func (s *Server) agentUpdate(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ns := r.FormValue("namespace")
	if ns == "" {
		ns = "claw-agents"
	}
	name, base, prompt := r.FormValue("name"), r.FormValue("baseImageRef"), r.FormValue("prompt")
	var agent clawv1alpha1.Agent
	if err := s.K8s.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &agent); err != nil {
		writeErr(w, http.StatusNotFound, "agent not found")
		return
	}
	if base != "" {
		agent.Spec.BaseImageRef = base
	}
	if agent.Spec.Model == nil {
		agent.Spec.Model = &clawv1alpha1.ModelSpec{}
	}
	agent.Spec.Model.SystemPrompt = prompt
	if err := s.K8s.Update(r.Context(), &agent); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.Redirect(w, r, "/ui/agents", http.StatusSeeOther)
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
