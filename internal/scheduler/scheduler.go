// Package scheduler fires cron-triggered agent invocations. It is a leader-elected
// controller-runtime Runnable: on each tick it creates a run for every due
// schedule (the run engine launches it, and the agent's answer posts to the
// schedule's Slack channel via the normal output path).
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/robfig/cron/v3"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/traego/kube-claw/internal/store"
)

// Scheduler ticks the schedule table and enqueues due runs.
type Scheduler struct {
	Store    store.Store
	Interval time.Duration // tick interval (default 30s)
}

// NeedLeaderElection ensures only one replica fires schedules.
func (s *Scheduler) NeedLeaderElection() bool { return true }

func (s *Scheduler) Start(ctx context.Context) error {
	iv := s.Interval
	if iv <= 0 {
		iv = 30 * time.Second
	}
	lg := log.FromContext(ctx).WithName("scheduler")
	lg.Info("starting scheduler", "interval", iv.String())
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.tick(ctx, lg)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context, lg interface {
	Info(string, ...any)
	Error(error, string, ...any)
}) {
	now := time.Now().UTC()
	_ = s.Store.Tx(ctx, func(tx store.Tx) error {
		schedules, err := tx.ListSchedules()
		if err != nil {
			return err
		}
		for _, sc := range schedules {
			if !sc.Enabled {
				continue
			}
			parsed, err := cron.ParseStandard(sc.Cron)
			if err != nil {
				lg.Error(err, "invalid cron expression", "schedule", sc.ID, "cron", sc.Cron)
				continue
			}
			// First sight: set the next occurrence without firing immediately.
			if sc.NextRunAt == "" {
				sc.NextRunAt = parsed.Next(now).Format(time.RFC3339)
				_ = tx.SetSchedule(sc)
				continue
			}
			next, err := time.Parse(time.RFC3339, sc.NextRunAt)
			if err != nil {
				sc.NextRunAt = parsed.Next(now).Format(time.RFC3339)
				_ = tx.SetSchedule(sc)
				continue
			}
			if now.Before(next) {
				continue // not due yet
			}
			// Due: enqueue a run for the agent; the engine launches it and the
			// answer posts to the schedule's channel via postOutput.
			runID := "run-" + randHex()
			src, _ := json.Marshal(map[string]string{"trigger": "schedule", "channel": sc.Channel, "schedule": sc.ID})
			in, _ := json.Marshal(map[string]string{"text": sc.Prompt})
			if err := tx.CreateRun(store.Run{
				ID: runID, AgentNamespace: sc.AgentNamespace, AgentName: sc.AgentName,
				Phase: "Pending", Source: string(src), Input: string(in),
			}); err != nil {
				lg.Error(err, "create scheduled run", "schedule", sc.ID)
				continue
			}
			if err := tx.AppendAudit(store.AuditEvent{Type: "schedule.fired", RunID: runID, Actor: "scheduler",
				Detail: map[string]any{"schedule": sc.ID, "agent": sc.AgentName}}); err != nil {
				return err
			}
			sc.LastRunAt = now.Format(time.RFC3339)
			sc.NextRunAt = parsed.Next(now).Format(time.RFC3339)
			_ = tx.SetSchedule(sc)
			lg.Info("fired schedule", "schedule", sc.ID, "run", runID, "next", sc.NextRunAt)
		}
		return nil
	})
}

func randHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
