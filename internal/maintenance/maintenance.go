// Package maintenance runs the housekeeping a long-lived instance needs:
// pruning old incidents and reclaiming the space they occupied.
package maintenance

import (
	"context"
	"log/slog"
	"time"

	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/store"
)

// DefaultRetention is how long closed incidents are kept.
//
// Ninety days covers a quarter, which is the window most postmortem and review
// cycles work in. Anything older is history rather than operational data.
const DefaultRetention = 90 * 24 * time.Hour

// DefaultInterval is how often housekeeping runs. Nightly: a vacuum takes the
// write lock for its duration, and doing that hourly would interrupt paging
// for no benefit.
const DefaultInterval = 24 * time.Hour

// Runner performs periodic housekeeping.
type Runner struct {
	db        *store.DB
	clk       clock.Clock
	retention time.Duration
	interval  time.Duration
	log       *slog.Logger
}

// Options configures a Runner.
type Options struct {
	// Retention is how long closed incidents are kept. Zero disables pruning
	// entirely, which is a legitimate choice for an instance whose history
	// somebody cares about.
	Retention time.Duration
	Interval  time.Duration
	Logger    *slog.Logger
}

// New creates a Runner.
func New(db *store.DB, clk clock.Clock, opts Options) *Runner {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Runner{
		db: db, clk: clk,
		retention: opts.Retention,
		interval:  opts.Interval,
		log:       opts.Logger,
	}
}

// Run performs housekeeping on a timer until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	if r.retention <= 0 {
		r.log.Info("retention is disabled; incidents are kept indefinitely")
		<-ctx.Done()
		return nil
	}

	r.log.Info("maintenance started", "retention", r.retention, "interval", r.interval)
	defer r.log.Info("maintenance stopped")

	ticker := r.clk.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C():
			if err := r.RunOnce(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				r.log.Error("maintenance pass failed", "error", err)
			}
		}
	}
}

// RunOnce prunes and vacuums immediately.
//
// Exported so an operator can reclaim space on demand, and so tests can drive
// a pass without waiting on a ticker.
func (r *Runner) RunOnce(ctx context.Context) error {
	if r.retention <= 0 {
		return nil
	}
	cutoff := r.clk.Now().Add(-r.retention)

	res, err := r.db.Prune(ctx, cutoff)
	if err != nil {
		return err
	}
	if res.Incidents == 0 && res.Alerts == 0 {
		// Nothing was removed, so there is nothing for a vacuum to reclaim.
		// Skipping it avoids taking the write lock for no reason.
		return nil
	}

	reclaimed, err := r.db.Vacuum(ctx)
	if err != nil {
		return err
	}
	r.log.Info("pruned old incidents",
		"before", cutoff.Format(time.RFC3339),
		"incidents", res.Incidents,
		"orphan_alerts", res.Alerts,
		"bytes_reclaimed", reclaimed)
	return nil
}
