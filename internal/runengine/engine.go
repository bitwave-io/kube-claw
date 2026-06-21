// Package runengine processes runs from the store: it gates each run on secret
// grants, then launches a Job per ready run (DESIGN.md §22, §31).
//
// Gate (Phase 4): a run for an agent that requires secrets is launched only when
// a valid (non-revoked, correctly-bound) grant exists for every required secret.
// Otherwise the engine creates a SecretRequest (deduped per agent+secret) and
// marks the run Blocked. On approval a grant appears and the next tick launches.
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	clawv1alpha1 "github.com/traego/kube-claw/api/v1alpha1"
	"github.com/traego/kube-claw/internal/secrets"
	"github.com/traego/kube-claw/internal/store"
	"github.com/traego/kube-claw/internal/workloads"
)

// Engine launches Jobs for ready runs on an interval.
type Engine struct {
	Store         store.Store
	K8s           client.Client
	RunnerImage   string
	ControllerURL string
	Interval      time.Duration
}

func (e *Engine) NeedLeaderElection() bool { return true }

func (e *Engine) Start(ctx context.Context) error {
	if e.Interval <= 0 {
		e.Interval = 2 * time.Second
	}
	lg := logf.Log.WithName("runengine")
	lg.Info("starting run engine", "interval", e.Interval, "runnerImage", e.RunnerImage)
	t := time.NewTicker(e.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := e.tick(ctx); err != nil {
				lg.Error(err, "processing runs")
			}
		}
	}
}

func (e *Engine) tick(ctx context.Context) error {
	var todo []store.Run
	if err := e.Store.Tx(ctx, func(tx store.Tx) error {
		for _, phase := range []string{"Pending", "Blocked"} {
			rs, err := tx.ListRunsByPhase(phase, 20)
			if err != nil {
				return err
			}
			todo = append(todo, rs...)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, run := range todo {
		e.evaluate(ctx, run)
	}
	e.reapRunning(ctx)
	return nil
}

// reapRunning marks a Running run Failed when its Job has failed (otherwise a
// crashed/un-attestable runner would leave the run Running forever — review gap).
func (e *Engine) reapRunning(ctx context.Context) {
	var running []store.Run
	if err := e.Store.Tx(ctx, func(tx store.Tx) error {
		rs, err := tx.ListRunsByPhase("Running", 50)
		running = rs
		return err
	}); err != nil {
		return
	}
	lg := logf.Log.WithName("runengine")
	for _, run := range running {
		var job batchv1.Job
		err := e.K8s.Get(ctx, types.NamespacedName{Namespace: run.AgentNamespace, Name: workloads.RunJobName(run)}, &job)
		if apierrors.IsNotFound(err) || err != nil {
			continue // job gone (TTL) or transient — leave; output may still arrive
		}
		if job.Status.Failed > 0 && job.Status.Active == 0 {
			_ = e.Store.Tx(ctx, func(tx store.Tx) error {
				if err := tx.MarkRunFailed(run.ID); err != nil {
					return err
				}
				return tx.AppendAudit(store.AuditEvent{Type: "agentrun.failed", RunID: run.ID, Actor: "runengine",
					Detail: map[string]any{"reason": "job failed"}})
			})
			lg.Info("run marked failed (job failed)", "run", run.ID)
		}
	}
}

// evaluate gates a run on its agent's secret grants, then launches or blocks it.
func (e *Engine) evaluate(ctx context.Context, run store.Run) {
	lg := logf.Log.WithName("runengine").WithValues("run", run.ID, "agent", run.AgentName)

	var agent clawv1alpha1.Agent
	if err := e.K8s.Get(ctx, types.NamespacedName{Namespace: run.AgentNamespace, Name: run.AgentName}, &agent); err != nil {
		lg.Error(err, "load agent")
		return
	}
	digest := agent.Status.SelectedImageDigest
	specHash := agent.Status.AgentSpecHash

	ready := true
	for _, sref := range agent.Spec.Secrets {
		dHash := secrets.DeliveryHash(sref.Delivery.Path, sref.Delivery.Mode, sref.Delivery.Env)
		granted, err := e.ensureGrantOrRequest(ctx, run, agent.Namespace, agent.Name, sref.Name, digest, specHash, dHash)
		if err != nil {
			lg.Error(err, "evaluate secret", "secret", sref.Name)
			return
		}
		if !granted {
			ready = false
		}
	}

	if !ready {
		if run.Phase != "Blocked" {
			_ = e.Store.Tx(ctx, func(tx store.Tx) error {
				if err := tx.MarkRunBlocked(run.ID); err != nil {
					return err
				}
				return tx.AppendAudit(store.AuditEvent{Type: "agentrun.blocked", RunID: run.ID, Actor: "runengine"})
			})
			lg.Info("run blocked on secret approval")
		}
		return
	}
	e.launch(ctx, run, &agent)
}

// resolveImage picks the image for a run's Job: a registered base image
// (baseImageRef) wins, then the agent's inline image, then the global fallback.
func (e *Engine) resolveImage(ctx context.Context, agent *clawv1alpha1.Agent) string {
	if agent.Spec.BaseImageRef != "" {
		var img string
		_ = e.Store.Tx(ctx, func(tx store.Tx) error {
			if b, err := tx.GetBaseImage(agent.Spec.BaseImageRef); err == nil {
				img = b.Image
			}
			return nil
		})
		if img != "" {
			return img
		}
	}
	if agent.Spec.Image != "" {
		return agent.Spec.Image
	}
	return e.RunnerImage
}

// ensureGrantOrRequest reports whether a valid grant exists for the secret; if
// not, it ensures a Pending SecretRequest (deduped) and returns false.
func (e *Engine) ensureGrantOrRequest(ctx context.Context, run store.Run, ns, agent, secretName, digest, specHash, deliveryHash string) (bool, error) {
	granted := false
	err := e.Store.Tx(ctx, func(tx store.Tx) error {
		sec, err := tx.GetSecret(ns, secretName)
		if errors.Is(err, store.ErrNotFound) {
			return nil // secret not created yet → not granted, no request we can reference
		}
		if err != nil {
			return err
		}
		if _, gerr := tx.FindValidGrant(ns, agent, sec.ID, digest, specHash, deliveryHash); gerr == nil {
			granted = true
			return nil
		} else if !errors.Is(gerr, store.ErrNotFound) {
			return gerr
		}
		// no valid grant → ensure a pending request
		exists, err := tx.PendingRequestExists(ns, agent, sec.ID)
		if err != nil || exists {
			return err
		}
		req := store.SecretRequest{
			ID: secrets.NewID("req"), Status: "Pending",
			AgentNamespace: ns, AgentName: agent, RunID: run.ID,
			SecretID: sec.ID, SecretName: secretName, ImageDigest: digest,
		}
		if err := tx.CreateSecretRequest(req); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{Type: "secret.requested", SecretID: sec.ID, RunID: run.ID, Actor: "runengine"})
	})
	return granted, err
}

func (e *Engine) launch(ctx context.Context, run store.Run, agent *clawv1alpha1.Agent) {
	lg := logf.Log.WithName("runengine").WithValues("run", run.ID, "agent", run.AgentName)

	image := e.resolveImage(ctx, agent)
	job := workloads.BuildRunJob(run, image, e.ControllerURL, inputText(run.Input))
	if err := e.K8s.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		lg.Error(err, "create job")
		return
	}
	if err := e.Store.Tx(ctx, func(tx store.Tx) error {
		if err := tx.MarkRunRunning(run.ID, workloads.RunJobName(run)); err != nil {
			return err
		}
		return tx.AppendAudit(store.AuditEvent{
			Type: "agentrun.started", RunID: run.ID, Actor: "runengine",
			Detail: map[string]any{"job": workloads.RunJobName(run), "agent": run.AgentName},
		})
	}); err != nil {
		lg.Error(err, "mark run running")
		return
	}
	lg.Info("launched run job", "job", workloads.RunJobName(run))
}

func inputText(input string) string {
	var in struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(input), &in)
	return in.Text
}
