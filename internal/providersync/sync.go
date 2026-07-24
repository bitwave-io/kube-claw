// Package providersync periodically refreshes each registered LLM provider's
// model catalog into the registry. It is a leader-elected controller-runtime
// Runnable (like internal/scheduler): on each tick it re-syncs every enabled
// provider whose catalog is older than the refresh interval, so newly released
// models show up without an admin hand-entering them.
package providersync

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/traego/kube-claw/internal/models"
)

// Syncer ticks the providers table and refreshes stale catalogs.
type Syncer struct {
	Models *models.Service
	// Interval is both the tick cadence and the staleness threshold (a provider
	// is re-synced when its last sync is older than this). Default 6h.
	Interval time.Duration
}

// NeedLeaderElection ensures only one replica refreshes catalogs.
func (s *Syncer) NeedLeaderElection() bool { return true }

func (s *Syncer) Start(ctx context.Context) error {
	iv := s.Interval
	if iv <= 0 {
		iv = 6 * time.Hour
	}
	lg := log.FromContext(ctx).WithName("providersync")
	lg.Info("starting provider catalog sync", "interval", iv.String())
	// Tick more often than the staleness window so a newly-added provider syncs
	// promptly, but never faster than every few minutes.
	tick := iv / 4
	if tick < time.Minute {
		tick = time.Minute
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	s.sweep(ctx, lg, iv) // initial pass at startup
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.sweep(ctx, lg, iv)
		}
	}
}

func (s *Syncer) sweep(ctx context.Context, lg logr, staleAfter time.Duration) {
	providers, err := s.Models.ListProviders(ctx)
	if err != nil {
		lg.Error(err, "list providers")
		return
	}
	now := time.Now().UTC()
	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		if !stale(p.LastSyncedAt, now, staleAfter) {
			continue
		}
		if err := s.Models.SyncProvider(ctx, p.Name); err != nil {
			lg.Error(err, "sync provider", "provider", p.Name)
			continue
		}
		lg.Info("synced provider catalog", "provider", p.Name)
	}
}

// stale reports whether a provider's last sync is old enough to refresh. An
// unparseable or empty timestamp counts as stale (never synced).
func stale(lastSynced string, now time.Time, after time.Duration) bool {
	if lastSynced == "" {
		return true
	}
	ts, err := time.Parse(time.RFC3339Nano, lastSynced)
	if err != nil {
		return true
	}
	return now.Sub(ts) >= after
}

type logr interface {
	Info(string, ...any)
	Error(error, string, ...any)
}
