package apihttp

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/robfig/cron/v3"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/store"
)

func schedID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "sch-" + hex.EncodeToString(b)
}

type scheduleReq struct {
	Namespace string `json:"namespace"`
	Agent     string `json:"agent"`
	Cron      string `json:"cron"`
	Prompt    string `json:"prompt"`
	Channel   string `json:"channel"`
	Enabled   *bool  `json:"enabled"`
}

// createSchedule (POST /v1/schedules) registers a cron-triggered agent invocation.
func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	var req scheduleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.saveSchedule(r, req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "scheduled"})
}

func (s *Server) saveSchedule(r *http.Request, req scheduleReq) error {
	if req.Agent == "" || req.Cron == "" || req.Prompt == "" {
		return fmt.Errorf("agent, cron and prompt are required")
	}
	if _, err := cron.ParseStandard(req.Cron); err != nil {
		return fmt.Errorf("invalid cron expression: %w", err)
	}
	ns := req.Namespace
	if ns == "" {
		ns = "claw-agents"
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if e := tx.SetSchedule(store.Schedule{
			ID: schedID(), AgentNamespace: ns, AgentName: req.Agent, Cron: req.Cron,
			Prompt: req.Prompt, Channel: req.Channel, Enabled: enabled,
		}); e != nil {
			return e
		}
		return tx.AppendAudit(store.AuditEvent{Type: "schedule.created", Actor: "api",
			Detail: map[string]any{"agent": req.Agent, "cron": req.Cron}})
	})
}

// listSchedules (GET /v1/schedules).
func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	var out []store.Schedule
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListSchedules()
		out = got
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if out == nil {
		out = []store.Schedule{}
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteScheduleAPI (DELETE /v1/schedules/{id}).
func (s *Server) deleteScheduleAPI(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error { return tx.DeleteSchedule(id) }); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- admin UI ---

func (s *Server) schedulesPage(w http.ResponseWriter, r *http.Request) {
	var scheds []store.Schedule
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListSchedules()
		scheds = got
		return e
	})
	var agents clawv1alpha1.AgentList
	_ = s.Reader.List(r.Context(), &agents, client.InNamespace("claw-agents"))
	body := `<p class=mut>Cron-triggered agent invocations. At each occurrence the agent runs the prompt and posts the answer to the channel. Cron is standard 5-field (UTC), e.g. <code>0 9 * * *</code> = 09:00 daily.</p>
<table><tr><th>Agent</th><th>Cron</th><th>Channel</th><th>Prompt</th><th>Enabled</th><th>Last / Next (UTC)</th><th></th></tr>
{{range .D.Schedules}}<tr>
<td><code>{{.AgentName}}</code></td><td><code>{{.Cron}}</code></td><td>{{.Channel}}</td>
<td class=mut><span class=snip>{{.Prompt}}</span></td>
<td>{{if .Enabled}}yes{{else}}<span class=mut>no (awaiting approval)</span>{{end}}</td>
<td class=mut>{{.LastRunAt}}<br>{{.NextRunAt}}</td>
<td style=white-space:nowrap>{{if not .Enabled}}<form method=post action=/ui/schedules/enable style="margin:0 0 .25rem 0">
<input type=hidden name=id value="{{.ID}}"><button>Enable</button></form>{{end}}
<form method=post action=/ui/schedules/delete style=margin:0 onsubmit="return confirm('Delete this schedule?')">
<input type=hidden name=id value="{{.ID}}"><button style="background:#c5221f;border-color:#c5221f">Delete</button></form></td>
</tr>{{else}}<tr><td colspan=7 class=mut>No schedules yet.</td></tr>{{end}}</table>

<h2>New schedule</h2>
<form method=post action=/ui/schedules/create style="background:#fff;border:1px solid var(--line);border-radius:8px;padding:1rem;max-width:660px">
<label>Agent</label><br><select name=agent required style="width:100%">
{{range .D.Agents.Items}}<option value="{{.Name}}">{{.Name}}</option>{{end}}</select><br><br>
<label>Cron (5-field, UTC)</label><br><input name=cron required placeholder="0 9 * * *" style="width:100%"><br><br>
<label>Channel (Slack channel id to post to)</label><br><input name=channel placeholder="C0123ABCD" style="width:100%"><br><br>
<label>Prompt</label><br><textarea name=prompt required style="width:100%;height:5rem" placeholder="Summarize yesterday's activity and flag anything unusual."></textarea><br><br>
<label><input type=checkbox name=enabled value=true checked> Enabled</label><br><br>
<button>Create schedule</button>
</form>`
	s.renderDash(w, "schedules", "Schedules", body, map[string]any{"Schedules": scheds, "Agents": &agents})
}

func (s *Server) uiCreateSchedule(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	enabled := r.FormValue("enabled") == "true"
	req := scheduleReq{Agent: r.FormValue("agent"), Cron: r.FormValue("cron"),
		Prompt: r.FormValue("prompt"), Channel: r.FormValue("channel"), Enabled: &enabled}
	if err := s.saveSchedule(r, req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	http.Redirect(w, r, "/ui/schedules", http.StatusSeeOther)
}

func (s *Server) uiDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id := r.FormValue("id")
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error { return tx.DeleteSchedule(id) })
	http.Redirect(w, r, "/ui/schedules", http.StatusSeeOther)
}

// uiEnableSchedule approves an agent-requested (disabled) schedule by flipping it
// on. This is the break-glass approval for the create_schedule agent tool: the
// agent can only PROPOSE a disabled schedule; enabling it is a human action.
func (s *Server) uiEnableSchedule(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id := r.FormValue("id")
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		sc, e := tx.GetSchedule(id)
		if e != nil {
			return e
		}
		sc.Enabled = true
		if e := tx.SetSchedule(sc); e != nil {
			return e
		}
		return tx.AppendAudit(store.AuditEvent{Type: "schedule.enabled", Actor: "ui",
			Detail: map[string]any{"schedule": id, "agent": sc.AgentName}})
	})
	http.Redirect(w, r, "/ui/schedules", http.StatusSeeOther)
}
