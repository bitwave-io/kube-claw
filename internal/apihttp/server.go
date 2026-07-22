// Package apihttp serves the controller's HTTP API (DESIGN.md §13).
//
// Phase 2 testability slice: enough to smoke-test a deployed controller —
//
//	GET  /healthz        liveness
//	GET  /v1/agents      list Agents (proves the k8s client + reconciler)
//	POST /v1/runs        create a run (proves the SQLite write path + audit)
//	GET  /v1/runs/{id}   read it back (proves persistence on the PVC)
//
// Auth (SA-token / claw session token) and TLS are layered in later phases; for
// now this is a cluster-internal, port-forwarded surface.
package apihttp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/approvals"
	"github.com/traego/kube-claw/internal/artifacts"
	"github.com/traego/kube-claw/internal/connector"
	"github.com/traego/kube-claw/internal/gitrepo"
	"github.com/traego/kube-claw/internal/identity"
	slackrouter "github.com/traego/kube-claw/internal/router/slack"
	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
)

// Server is a controller-runtime Runnable that serves the HTTP API.
type Server struct {
	Addr      string
	Store     store.Store
	Reader    client.Reader         // uncached k8s reader (mgr.GetAPIReader)
	K8s       client.Client         // writer, for creating Agent CRs from the API
	Secrets   *secrets.Service      // secret authority
	UIBase    string                // base URL of the intake UI (for returned links)
	Identity  identity.Provider     // /login credential verifier
	Signer    *identity.Signer      // claw session token signer
	Approvals *approvals.Service    // shared approval path
	Artifacts *artifacts.Service    // published documents + share links
	GitRepos  *gitrepo.Service      // git-repo access approval authority
	Router    *slackrouter.Router   // connector routing (nil if no routes configured)
	Notifier  *slackrouter.Notifier // posts replies/approvals to Slack (nil if no bot token)
	Deliverer *connector.Deliverer  // pushes run events to connector callbacks (nil = default)
	// AdminPassword gates the admin dashboard (/ui/*) with HTTP basic auth
	// (user "admin"). Empty = no auth (local/dev). The secret-intake UI runs on a
	// separate listener and is never gated here (it's one-time-token protected).
	AdminPassword string
	// EnableFakeSlackEvents registers the test-only POST /v1/connectors/slack/events
	// endpoint. Off in prod so callers can't simulate Slack-triggered runs.
	EnableFakeSlackEvents bool
	// Upgrades exposes the self-update coordinator (nil = self-update off).
	Upgrades UpgradeAPI
}

// NeedLeaderElection lets the API run on every replica (false = not gated).
func (s *Server) NeedLeaderElection() bool { return false }

// Start runs the HTTP server until ctx is cancelled (manager.Runnable).
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()
	logf.Log.WithName("apihttp").Info("serving HTTP API", "addr", s.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/agents", s.listAgents)
	mux.HandleFunc("POST /v1/agents", s.createAgent)
	mux.HandleFunc("POST /v1/runs", s.createRun)
	mux.HandleFunc("GET /v1/runs", s.listRuns)
	mux.HandleFunc("GET /v1/runs/{id}", s.getRun)
	mux.HandleFunc("POST /v1/runs/{id}/outputs", s.postOutput)
	mux.HandleFunc("POST /v1/runs/{id}/artifacts", s.publishArtifact)
	mux.HandleFunc("POST /v1/runs/{id}/register-secret", s.agentRegisterSecret)
	mux.HandleFunc("POST /v1/runs/{id}/announce-channel", s.agentSetAnnounceChannel)
	mux.HandleFunc("POST /v1/runs/{id}/progress", s.postProgress)
	mux.HandleFunc("POST /v1/secrets", s.createSecret)
	mux.HandleFunc("GET /v1/secrets/{name}/metadata", s.secretMetadata)
	mux.HandleFunc("PUT /v1/secrets/{name}/versions", s.putSecretVersion)
	mux.HandleFunc("GET /v1/secret-requests", s.listRequests)
	mux.HandleFunc("POST /v1/secret-requests/{id}/approve", s.approveRequest)
	mux.HandleFunc("POST /v1/secret-requests/{id}/deny", s.denyRequest)
	mux.HandleFunc("GET /v1/secret-grants", s.listGrants)
	mux.HandleFunc("POST /v1/secret-grants/{id}/revoke", s.revokeGrant)
	mux.HandleFunc("POST /v1/login", s.login)
	mux.HandleFunc("POST /v1/token/refresh", s.refreshToken)
	mux.HandleFunc("POST /v1/runs/{id}/materialize", s.materialize)
	mux.HandleFunc("POST /v1/base-images", s.createBaseImage)
	mux.HandleFunc("GET /v1/base-images", s.listBaseImages)
	mux.HandleFunc("GET /ui/base-images", s.baseImagesPage)
	mux.HandleFunc("POST /ui/base-images", s.baseImagesSubmit)
	// Fake Slack event endpoint — simulates a Slack-triggered run without the
	// Slack transport. Test-only; off unless explicitly enabled (CLAW_ENABLE_FAKE_SLACK).
	if s.EnableFakeSlackEvents {
		mux.HandleFunc("POST /v1/connectors/slack/events", s.slackEvent)
	}
	// Connector plane: management is admin-gated (adminOK), ingest is
	// API-key-gated — the one /v1 surface meant for public exposure.
	mux.HandleFunc("POST /v1/connectors", s.createConnector)
	mux.HandleFunc("GET /v1/connectors", s.listConnectors)
	mux.HandleFunc("DELETE /v1/connectors/{id}", s.deleteConnector)
	mux.HandleFunc("POST /v1/connectors/{id}/rotate-key", s.rotateConnectorKey)
	mux.HandleFunc("POST /v1/connectors/{id}/messages", s.connectorIngest)
	mux.HandleFunc("GET /v1/sessions/{id}/history", s.sessionHistory)
	mux.HandleFunc("GET /v1/runs/{id}/available-secrets", s.availableSecrets)
	mux.HandleFunc("POST /v1/runs/{id}/request-secret", s.requestSecret)
	mux.HandleFunc("GET /v1/runs/{id}/requested-secret", s.requestedSecret)
	mux.HandleFunc("POST /v1/runs/{id}/request-schedule", s.requestSchedule)
	mux.HandleFunc("GET /v1/runs/{id}/schedules", s.runSchedules)
	mux.HandleFunc("POST /v1/gitrepos", s.createGitRepo)
	mux.HandleFunc("GET /v1/gitrepos", s.listGitRepos)
	mux.HandleFunc("DELETE /v1/gitrepos/{name}", s.deleteGitRepo)
	mux.HandleFunc("GET /v1/gitrepo-requests", s.listGitRepoRequests)
	mux.HandleFunc("POST /v1/gitrepo-requests/{id}/approve", s.approveGitRepoRequest)
	mux.HandleFunc("POST /v1/gitrepo-requests/{id}/deny", s.denyGitRepoRequest)
	mux.HandleFunc("GET /v1/gitrepo-grants", s.listGitRepoGrants)
	mux.HandleFunc("POST /v1/gitrepo-grants/{id}/revoke", s.revokeGitRepoGrant)
	mux.HandleFunc("GET /v1/runs/{id}/available-gitrepos", s.availableGitRepos)
	mux.HandleFunc("POST /v1/runs/{id}/request-gitrepo", s.requestGitRepo)
	mux.HandleFunc("GET /v1/runs/{id}/requested-gitrepo", s.requestedGitRepo)
	mux.HandleFunc("POST /v1/sessions/{id}/claim-next", s.claimNextTurn)
	mux.HandleFunc("POST /v1/sessions/{id}/sleep", s.sessionSleep)
	mux.HandleFunc("GET /v1/prompts", s.listPrompts)
	mux.HandleFunc("PUT /v1/prompts", s.setPrompt)
	mux.HandleFunc("GET /v1/prompts/{ns}/{name}", s.getPrompt)
	mux.HandleFunc("GET /ui/prompts", s.promptsPage)
	mux.HandleFunc("POST /ui/prompts", s.promptsSubmit)
	mux.HandleFunc("GET /v1/version", s.getVersion)
	mux.HandleFunc("GET /v1/upgrade/status", s.upgradeStatus)
	mux.HandleFunc("POST /v1/upgrade/approve", s.upgradeApprove)
	mux.HandleFunc("POST /v1/upgrade/check", s.upgradeCheck)
	mux.HandleFunc("GET /v1/settings", s.listSettings)
	mux.HandleFunc("PUT /v1/settings/{key}", s.setSetting)
	mux.HandleFunc("GET /v1/schedules", s.listSchedules)
	mux.HandleFunc("POST /v1/schedules", s.createSchedule)
	mux.HandleFunc("DELETE /v1/schedules/{id}", s.deleteScheduleAPI)
	mux.HandleFunc("GET /ui/schedules", s.schedulesPage)
	mux.HandleFunc("POST /ui/schedules/create", s.uiCreateSchedule)
	mux.HandleFunc("POST /ui/schedules/enable", s.uiEnableSchedule)
	mux.HandleFunc("POST /ui/schedules/delete", s.uiDeleteSchedule)
	mux.HandleFunc("GET /ui/logo.png", s.logo)
	mux.HandleFunc("GET /ui", s.dashboardHome)
	mux.HandleFunc("GET /ui/dashboard", s.dashboardHome)
	mux.HandleFunc("GET /ui/secrets", s.secretsPage)
	mux.HandleFunc("POST /ui/secrets/rotate", s.rotateSecret)
	mux.HandleFunc("POST /ui/secrets/delete", s.deleteSecret)
	mux.HandleFunc("GET /ui/requests", s.requestsPage)
	mux.HandleFunc("POST /ui/requests/approve", s.uiApproveRequest)
	mux.HandleFunc("POST /ui/requests/deny", s.uiDenyRequest)
	mux.HandleFunc("GET /ui/gitrepos", s.gitReposPage)
	mux.HandleFunc("POST /ui/gitrepos/create", s.uiCreateGitRepo)
	mux.HandleFunc("POST /ui/gitrepos/delete", s.uiDeleteGitRepo)
	mux.HandleFunc("POST /ui/gitrepo-requests/approve", s.uiApproveGitRepoRequest)
	mux.HandleFunc("POST /ui/gitrepo-requests/deny", s.uiDenyGitRepoRequest)
	mux.HandleFunc("GET /ui/audit", s.auditPage)
	mux.HandleFunc("GET /ui/conversations", s.conversationsPage)
	mux.HandleFunc("GET /ui/agents", s.agentsPage)
	mux.HandleFunc("POST /ui/agents/idle", s.agentSetIdle)
	mux.HandleFunc("POST /ui/agents/create", s.uiCreateAgent)
	mux.HandleFunc("GET /ui/agents/edit", s.agentEditPage)
	mux.HandleFunc("POST /ui/agents/update", s.agentUpdate)
	mux.HandleFunc("GET /ui/channels", s.channelsPage)
	mux.HandleFunc("GET /ui/settings", s.settingsPage)
	mux.HandleFunc("POST /ui/settings", s.uiSetSettings)
	mux.HandleFunc("POST /ui/settings/check-upgrades", s.uiCheckUpgrades)
	return s.withAdminAuth(mux)
}

// authRunInSession verifies the caller's run session token and that runID is the
// token's run OR a run in the same session (warm-session pods reuse one token
// across follow-up turns). Used to authenticate runner callbacks.
func (s *Server) authRunInSession(r *http.Request, runID string) bool {
	claims, err := s.Signer.Verify(bearer(r))
	if err != nil {
		return false
	}
	if claims.RunID == runID {
		return true
	}
	ok := false
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		caller, e := tx.GetRun(claims.RunID)
		if e != nil {
			return nil
		}
		target, e := tx.GetRun(runID)
		if e != nil {
			return nil
		}
		if caller.SessionID != "" && caller.SessionID == target.SessionID {
			ok = true
		}
		return nil
	})
	return ok
}

// authSession verifies the caller's token belongs to a run in the given session.
func (s *Server) authSession(r *http.Request, sessionID string) bool {
	claims, err := s.Signer.Verify(bearer(r))
	if err != nil {
		return false
	}
	ok := false
	_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
		caller, e := tx.GetRun(claims.RunID)
		if e == nil && caller.SessionID == sessionID {
			ok = true
		}
		return nil
	})
	return ok
}

// withAdminAuth gates every /ui/* admin route with HTTP basic auth when an admin
// password is configured. /v1/*, /healthz, etc. are unaffected; the secret-intake
// UI is a separate listener and never reaches this handler.
func (s *Server) withAdminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.AdminPassword != "" && strings.HasPrefix(r.URL.Path, "/ui") {
			u, p, ok := r.BasicAuth()
			if !ok || u != "admin" || subtle.ConstantTimeCompare([]byte(p), []byte(s.AdminPassword)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="kube-claw admin"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type agentView struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Digest    string `json:"digest"`
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	var list clawv1alpha1.AgentList
	if err := s.Reader.List(r.Context(), &list); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]agentView, 0, len(list.Items))
	for _, a := range list.Items {
		out = append(out, agentView{a.Namespace, a.Name, a.Status.Phase, a.Status.SelectedImageDigest})
	}
	writeJSON(w, http.StatusOK, out)
}

type createRunReq struct {
	Namespace string `json:"namespace"`
	Agent     string `json:"agent"`
	Input     string `json:"input"`
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var req createRunReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Agent == "" || req.Namespace == "" {
		writeErr(w, http.StatusBadRequest, "namespace and agent are required")
		return
	}
	run := store.Run{
		ID:             newRunID(),
		AgentNamespace: req.Namespace,
		AgentName:      req.Agent,
		Phase:          "Pending",
		Input:          mustJSON(map[string]string{"text": req.Input}),
		Source:         mustJSON(map[string]string{"trigger": "cli"}),
		CreatedAt:      store.NowRFC3339(),
	}
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		if err := tx.CreateRun(run); err != nil {
			return err
		}
		// Audit in the same tx (the store invariant).
		return tx.AppendAudit(store.AuditEvent{
			Type:  "agentrun.created",
			RunID: run.ID,
			Actor: "cli",
			Detail: map[string]any{
				"agent":     req.Agent,
				"namespace": req.Namespace,
			},
		})
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": run.ID, "phase": run.Phase})
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	var runs []store.Run
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListRuns(100)
		runs = got
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if runs == nil {
		runs = []store.Run{}
	}
	writeJSON(w, http.StatusOK, runs)
}

type runView struct {
	store.Run
	Outputs []store.Output `json:"outputs"`
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var view runView
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		run, e := tx.GetRun(id)
		if e != nil {
			return e
		}
		outs, e := tx.ListOutputs(id)
		if e != nil {
			return e
		}
		view = runView{Run: run, Outputs: outs}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if view.Outputs == nil {
		view.Outputs = []store.Output{}
	}
	writeJSON(w, http.StatusOK, view)
}

type postOutputReq struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// postProgress posts an intermediate, in-thread status update for a running turn
// (run not completed) so long operations report progress. The update is also
// recorded as a "progress" output row: it marks the Slack thread as in-use (so
// postOutput keeps the final reply there) and shows in the run view mid-turn.
func (s *Server) postProgress(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.authRunInSession(r, id) {
		writeErr(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		writeErr(w, http.StatusBadRequest, "text required")
		return
	}
	var run store.Run
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetRun(id)
		if e != nil {
			return e
		}
		run = got
		return tx.AppendOutput(id, store.Output{Kind: "progress", Content: req.Text})
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.Notifier != nil {
		if ch := slackrouter.SlackChannel(run.Source); ch != "" {
			sessTS := run.SessionID
			if slackrouter.SlackIsDM(run.Source) {
				sessTS = "" // a DM session id is the IM channel, not a thread ts
			}
			if e := s.Notifier.PostReply(r.Context(), ch, sessTS, req.Text); e != nil {
				logf.Log.WithName("apihttp").Error(e, "post slack progress", "run", id)
			}
		}
	}
	if cid := connector.SourceConnectorID(run.Source); cid != "" {
		s.deliverToConnector(cid, run, "progress", req.Text)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "posted"})
}

// postOutput records a runner's output and marks the run Succeeded. This is the
// runner→controller callback (DESIGN.md §36). Auth (claw session token) lands in
// Phase 5; for now it is unauthenticated on the cluster-internal API.
func (s *Server) postOutput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.authRunInSession(r, id) {
		writeErr(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	var req postOutputReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Kind == "" {
		req.Kind = "text"
	}
	var run store.Run
	alreadyDone := false
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetRun(id)
		if e != nil {
			return e
		}
		run = got
		// Idempotency: a completed run's reply must not post to Slack twice
		// (runner retry after a timed-out-but-delivered POST, Job backoff
		// replacement pods, claim/launch races all re-send outputs).
		if run.Phase == "Succeeded" {
			alreadyDone = true
			return nil
		}
		if e := tx.AppendOutput(id, store.Output{Kind: req.Kind, Content: req.Content}); e != nil {
			return e
		}
		if e := tx.MarkRunSucceeded(id); e != nil {
			return e
		}
		return tx.AppendAudit(store.AuditEvent{Type: "agentrun.completed", RunID: id, Actor: "runner"})
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if alreadyDone {
		logf.Log.WithName("apihttp").Info("duplicate output ignored — run already succeeded", "run", id)
		writeJSON(w, http.StatusOK, map[string]string{"status": "already recorded"})
		return
	}
	// A "none" output is the agent declining to reply (the message wasn't for
	// it, or the run was coalesced into a newer turn): the run is completed and
	// the 👀 marker cleared above/below, but nothing is posted anywhere.
	if req.Kind == "none" {
		if s.Notifier != nil {
			if ch := slackrouter.SlackChannel(run.Source); ch != "" {
				if ts := slackrouter.SlackEventTS(run.Source); ts != "" {
					_ = s.Notifier.RemoveReaction(r.Context(), ch, ts, "eyes")
				}
			}
		}
		// A connector accepted this run with 202 {runId} — it must still get a
		// terminal event, or a coalesced/declined run looks stuck forever from
		// the outside. "none" = the run completed with no reply (absorbed into a
		// newer turn's answer, or the agent judged no reply was needed).
		if cid := connector.SourceConnectorID(run.Source); cid != "" {
			s.deliverToConnector(cid, run, "none", "")
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
		return
	}
	// #2: post the agent's reply back to Slack, and clear the 👀 on the
	// triggering message. Channel config decides thread vs in-channel, but an
	// already-threaded conversation always stays in its thread.
	if s.Notifier != nil {
		if ch := slackrouter.SlackChannel(run.Source); ch != "" {
			threadTS := run.SessionID // default: reply in-thread
			if slackrouter.SlackIsDM(run.Source) {
				threadTS = "" // DM replies post straight to the IM channel
			} else {
				_ = s.Store.Tx(r.Context(), func(tx store.Tx) error {
					if replyTopLevel(tx, run, ch) {
						threadTS = ""
					}
					return nil
				})
			}
			if e := s.Notifier.PostReply(r.Context(), ch, threadTS, req.Content); e != nil {
				logf.Log.WithName("apihttp").Error(e, "post slack reply", "run", id)
			}
			if ts := slackrouter.SlackEventTS(run.Source); ts != "" {
				_ = s.Notifier.RemoveReaction(r.Context(), ch, ts, "eyes")
			}
		}
	}
	// Connector-triggered runs push the answer to the registered callback URL
	// (the webhook twin of the Slack reply above).
	if cid := connector.SourceConnectorID(run.Source); cid != "" {
		s.deliverToConnector(cid, run, "output", req.Content)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

// replyTopLevel reports whether a run's final answer may post to the channel
// top-level instead of its thread. The channel config must allow in-channel
// replies AND the conversation must not already be threaded — once anything
// lives in the thread (the trigger was itself a thread reply, earlier turns ran
// in this session, or this run posted progress there), the answer stays with
// it. Splitting one conversation between a thread and the channel is worse
// than either mode alone.
func replyTopLevel(tx store.Tx, run store.Run, channel string) bool {
	if run.SessionID == "" {
		return true // no thread to reply into
	}
	cfg, err := tx.GetChannelConfig(channel)
	if err != nil || cfg.ThreadOnly {
		return false
	}
	if ts := slackrouter.SlackEventTS(run.Source); ts != "" && ts != run.SessionID {
		return false // triggered from inside an existing thread
	}
	if runs, err := tx.ListRunsBySession(run.SessionID, 2); err != nil || len(runs) > 1 {
		return false // multi-turn conversation — it lives in the thread
	}
	outs, err := tx.ListOutputs(run.ID)
	if err != nil {
		return false
	}
	for _, o := range outs {
		if o.Kind == "progress" {
			return false // progress updates already went to the thread
		}
	}
	return true
}

type publishArtifactReq struct {
	ArtifactID string `json:"artifactId"` // set to reshare an existing document
	Title      string `json:"title"`
	Content    string `json:"content"` // markdown
	TTL        string `json:"ttl"`     // Go duration, e.g. "24h"; empty = default
}

// publishArtifact is the runner→controller callback behind the agent's
// publish_document tool: it stores a document (or reuses one on reshare) and
// returns a time-bound public share link. Authenticated like the other runner
// callbacks — this does NOT widen the public surface; only GET /d/{token} on
// the separate UI listener is public.
func (s *Server) publishArtifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.authRunInSession(r, id) {
		writeErr(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	var req publishArtifactReq
	// The wire form is JSON: escaping can expand content up to 6x (\uXXXX for
	// control chars; \\n, \" for the common cases), so the raw-body cap must be
	// 6*MaxContentBytes or a legal document gets truncated into invalid JSON.
	// The DECODED content is still held to MaxContentBytes by Publish.
	if err := json.NewDecoder(io.LimitReader(r.Body, 6*artifacts.MaxContentBytes+64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var ttl time.Duration
	if req.TTL != "" {
		d, err := time.ParseDuration(req.TTL)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid ttl (use a Go duration, e.g. \"24h\")")
			return
		}
		ttl = d
	}
	var run store.Run
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetRun(id)
		run = got
		return e
	}); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	pub, err := s.Artifacts.Publish(r.Context(), id, run.SessionID, req.ArtifactID, req.Title, req.Content, ttl)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, artifacts.ErrWrongSession):
		// One 404 for both — an agent probing other sessions' artifact ids learns nothing.
		writeErr(w, http.StatusNotFound, "artifact not found")
		return
	case errors.Is(err, artifacts.ErrContentTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"artifactId": pub.ArtifactID,
		"url":        s.UIBase + "/d/" + pub.Token,
		"expiresAt":  pub.ExpiresAt.Format(time.RFC3339),
	})
}

// agentRegisterSecret is the runner→controller callback behind the agent's
// register_secret tool — the conversational successor to the deterministic
// "register secret" DM command (which still works). It creates the secret with
// the REQUESTING SLACK USER as granter and mints a one-time intake link; the
// agent (and the controller) never see the value. Slack-originated runs only:
// the human in the conversation is the authority the grant hangs off.
func (s *Server) agentRegisterSecret(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.authRunInSession(r, id) {
		writeErr(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	var req struct{ Name, Description string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	var run store.Run
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetRun(id)
		run = got
		return e
	}); err != nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	user := slackrouter.SlackUser(run.Source)
	if user == "" {
		writeErr(w, http.StatusForbidden, "only Slack-originated runs can register secrets (the requesting user becomes the approver)")
		return
	}
	if s.Secrets == nil {
		writeErr(w, http.StatusServiceUnavailable, "secret registration isn't configured on this controller")
		return
	}
	const ns = "claw-agents" // parity with the DM command
	_, _ = s.Secrets.CreateSecret(r.Context(), ns, req.Name, "", req.Description, []string{user})
	tok, err := s.Secrets.MintIntakeToken(r.Context(), ns, req.Name, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"name": req.Name, "granter": user,
		"url": s.UIBase + "/ui/secret-intake/" + tok,
	})
}

// slackChannelID validates an encoded Slack channel id for the
// announce-channel tool.
var slackChannelID = regexp.MustCompile(`^C[A-Z0-9]+$`)

// agentSetAnnounceChannel is the runner→controller callback behind the agent's
// set_release_announce_channel tool. Same authority rule as the DM command:
// once an upgrade admin is claimed, only the admin may move it.
func (s *Server) agentSetAnnounceChannel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.authRunInSession(r, id) {
		writeErr(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	var req struct{ Channel string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Channel != "" && !slackChannelID.MatchString(req.Channel) {
		writeErr(w, http.StatusBadRequest, "not a Slack channel id (want C…; extract it from the <#C…|name> reference)")
		return
	}
	var run store.Run
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetRun(id)
		run = got
		return e
	}); err != nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	user := slackrouter.SlackUser(run.Source)
	if user == "" {
		writeErr(w, http.StatusForbidden, "only Slack-originated runs can change the management channel")
		return
	}
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		admin, e := tx.GetSetting(store.SettingUpgradeAdmin)
		if e != nil && !errors.Is(e, store.ErrNotFound) {
			return e
		}
		if admin != "" && admin != user {
			return fmt.Errorf("only the upgrade admin (<@%s>) can change where releases are announced", admin)
		}
		return tx.SetSetting(store.SettingMgmtChannel, req.Channel)
	})
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	status := "set"
	if req.Channel == "" {
		status = "cleared"
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status, "channel": req.Channel})
}

// --- secrets (Phase 3) ---

type createSecretReq struct {
	Namespace   string   `json:"namespace"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Granters    []string `json:"granters"`
}

// createSecret records metadata + granters and mints a one-time intake link.
func (s *Server) createSecret(w http.ResponseWriter, r *http.Request) {
	var req createSecretReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Namespace == "" || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "namespace and name are required")
		return
	}
	sec, err := s.Secrets.CreateSecret(r.Context(), req.Namespace, req.Name, req.Type, req.Description, req.Granters)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	tok, err := s.Secrets.MintIntakeToken(r.Context(), req.Namespace, req.Name, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	path := "/ui/secret-intake/" + tok
	writeJSON(w, http.StatusCreated, map[string]string{
		"id":         sec.ID,
		"intakePath": path,
		"intakeURL":  s.UIBase + path,
	})
}

func (s *Server) secretMetadata(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	name := r.PathValue("name")
	var sec store.Secret
	err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.GetSecret(ns, name)
		sec = got
		return e
	})
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "secret not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sec) // metadata only — never the value
}

// putSecretVersion is the break-glass / CI value upload (DESIGN.md §8.3).
func (s *Server) putSecretVersion(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	name := r.PathValue("name")
	value, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read body")
		return
	}
	if len(value) == 0 {
		writeErr(w, http.StatusBadRequest, "empty value")
		return
	}
	if err := s.Secrets.PutValue(r.Context(), ns, name, value, "cli"); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "secret not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stored"})
}

// slackEventReq simulates a Slack message (the fake event-ingestion endpoint,
// DESIGN.md §32). The real Socket Mode transport drives the same Router path.
type slackEventReq struct {
	EventID   string `json:"eventId"`
	Channel   string `json:"channel"`
	SessionID string `json:"sessionId"`
	Text      string `json:"text"`
	Mentioned bool   `json:"mentioned"`
	User      string `json:"user"`
}

func (s *Server) slackEvent(w http.ResponseWriter, r *http.Request) {
	if s.Router == nil {
		writeErr(w, http.StatusServiceUnavailable, "no slack routes configured")
		return
	}
	var req slackEventReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Channel == "" || req.EventID == "" {
		writeErr(w, http.StatusBadRequest, "eventId and channel are required")
		return
	}
	runID, err := s.Router.HandleMessage(r.Context(), req.EventID, req.Channel, req.SessionID, req.Text, req.Mentioned, req.User)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if runID == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored (no route match or duplicate)"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"runId": runID})
}

// --- approval (Phase 4) ---

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	var reqs []store.SecretRequest
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListSecretRequests(status)
		reqs = got
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if reqs == nil {
		reqs = []store.SecretRequest{}
	}
	writeJSON(w, http.StatusOK, reqs)
}

type approveReq struct {
	Approver string `json:"approver"`
	Reason   string `json:"reason"`
}

func (s *Server) approveRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body approveReq
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Approver == "" {
		body.Approver = "cli"
	}

	// Break-glass approval (the API/CLI caller is already trusted).
	grant, err := s.Approvals.Approve(r.Context(), id, body.Approver, body.Reason)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "request not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"grant": grant.ID, "status": "approved"})
}

func (s *Server) denyRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body approveReq
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.Secrets.DenyRequest(r.Context(), id, body.Approver, body.Reason); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "request not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "denied"})
}

func (s *Server) listGrants(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("namespace")
	agent := r.URL.Query().Get("agent")
	var grants []store.Grant
	if err := s.Store.Tx(r.Context(), func(tx store.Tx) error {
		got, e := tx.ListGrants(ns, agent)
		grants = got
		return e
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if grants == nil {
		grants = []store.Grant{}
	}
	writeJSON(w, http.StatusOK, grants)
}

func (s *Server) revokeGrant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body approveReq
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.Secrets.RevokeGrant(r.Context(), id, body.Approver, body.Reason); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "grant not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// --- helpers ---

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "run-" + hex.EncodeToString(b)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
