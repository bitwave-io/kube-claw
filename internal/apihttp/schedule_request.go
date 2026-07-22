package apihttp

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/robfig/cron/v3"

	slackrouter "github.com/traego/kube-claw/internal/router/slack"
	"github.com/traego/kube-claw/internal/store"
)

// requestScheduleReq is what the agent's create_schedule tool sends. The agent
// supplies only the WHAT (cron + prompt); the WHO (namespace/agent) and WHERE
// (channel) are taken from the run itself so an agent can't schedule work for a
// different agent or post to an arbitrary channel.
type requestScheduleReq struct {
	Cron    string `json:"cron"`
	Prompt  string `json:"prompt"`
	Channel string `json:"channel"` // optional override; defaults to the run's channel
}

// requestSchedule (POST /v1/runs/{id}/request-schedule) lets an agent PROPOSE a
// recurring invocation of itself. It is a gated action, mirroring the on-demand
// secret/gitrepo flow: the schedule is created DISABLED and the run's Slack user
// is DM'd an approval prompt. The scheduler skips disabled schedules, so nothing
// fires until a human enables it in the dashboard (/ui/schedules). This keeps an
// agent from silently arming unattended, recurring runs of itself.
func (s *Server) requestSchedule(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if claims, err := s.Signer.Verify(bearer(r)); err != nil || claims.RunID != runID {
		writeErr(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	var req requestScheduleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Cron == "" || req.Prompt == "" {
		writeErr(w, http.StatusBadRequest, "cron and prompt are required")
		return
	}
	if _, err := cron.ParseStandard(req.Cron); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid cron expression: "+err.Error())
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
	// The agent schedules ITSELF, in its own namespace, posting to this thread's
	// channel by default — none of which the agent gets to choose freely.
	ns, agentName := run.AgentNamespace, run.AgentName
	user := slackrouter.SlackUser(run.Source)
	channel := req.Channel
	if channel == "" {
		channel = slackrouter.SlackChannel(run.Source)
	}

	id := schedID()
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if e := tx.SetSchedule(store.Schedule{
			ID: id, AgentNamespace: ns, AgentName: agentName, Cron: req.Cron,
			Prompt: req.Prompt, Channel: channel, Enabled: false, // gated: off until a human approves
		}); e != nil {
			return e
		}
		return tx.AppendAudit(store.AuditEvent{Type: "schedule.requested", RunID: runID, Actor: agentName,
			Detail: map[string]any{"schedule": id, "cron": req.Cron, "requestedBy": user}})
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	notified := false
	if s.Notifier != nil && user != "" {
		msg := fmt.Sprintf(":calendar: *Schedule approval needed*\nAgent `%s` wants to run on a recurring schedule.", agentName)
		msg += "\n• When: `" + req.Cron + "` (UTC)"
		msg += "\n• It will run: " + req.Prompt
		if channel != "" {
			msg += fmt.Sprintf("\n• Posting to: <#%s>", channel)
		}
		msg += fmt.Sprintf("\n\nIt is OFF until you enable it. Review and enable it here:\n%s/ui/schedules", s.UIBase)
		if err := s.Notifier.PostReply(r.Context(), user, "", msg); err == nil {
			notified = true
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"status": "requested", "id": id, "enabled": false, "notified": notified,
	})
}

// runSchedules (GET /v1/runs/{id}/schedules) returns the schedules for the run's
// own agent, so an agent can see (and describe) what it already has scheduled.
// Scoped to the run's agent — it never sees other agents' schedules.
func (s *Server) runSchedules(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if claims, err := s.Signer.Verify(bearer(r)); err != nil || claims.RunID != runID {
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
	var out []store.Schedule
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		all, e := tx.ListSchedules()
		if e != nil {
			return e
		}
		for _, sc := range all {
			if sc.AgentNamespace == run.AgentNamespace && sc.AgentName == run.AgentName {
				out = append(out, sc)
			}
		}
		return nil
	})
	if out == nil {
		out = []store.Schedule{}
	}
	writeJSON(w, http.StatusOK, out)
}
