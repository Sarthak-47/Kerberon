// Package heartbeat implements dead-man's switches.
//
// A cron job or worker pings on a schedule; if a ping does not arrive within
// its expected interval plus a grace period, Kerberon raises an incident.
//
// This is how Kerberon catches what Prometheus cannot: a cron that silently
// stopped running, a backup that stopped completing, a worker that died
// without alerting. Nothing is emitting a metric to alert on, and the absence
// of a signal is the signal (spec section 8.6).
package heartbeat

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"database/sql"

	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/group"
	"github.com/Sarthak-47/kerberon/internal/store"
)

// DefaultSweepInterval is how often overdue switches are checked. Thirty
// seconds bounds how late a missed heartbeat can be noticed without polling
// hard enough to matter.
const DefaultSweepInterval = 30 * time.Second

// AlertSink receives the synthetic alerts a missed heartbeat produces.
//
// A missed heartbeat enters the system as an ordinary alert rather than
// creating an incident directly, so it inherits routing, grouping, escalation
// and everything else without a parallel code path.
type AlertSink interface {
	Ingest(ctx context.Context, alerts []core.Alert) (group.Result, error)
}

// Sweeper watches for overdue heartbeats.
type Sweeper struct {
	db       *store.DB
	clk      clock.Clock
	sink     AlertSink
	interval time.Duration
	log      *slog.Logger
}

// Options configures a Sweeper.
type Options struct {
	Interval time.Duration
	Logger   *slog.Logger
}

// New creates a Sweeper.
func New(db *store.DB, clk clock.Clock, sink AlertSink, opts Options) *Sweeper {
	if opts.Interval <= 0 {
		opts.Interval = DefaultSweepInterval
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Sweeper{db: db, clk: clk, sink: sink, interval: opts.Interval, log: opts.Logger}
}

// Run sweeps until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) error {
	s.log.Info("heartbeat sweeper started", "interval", s.interval)
	defer s.log.Info("heartbeat sweeper stopped")

	ticker := s.clk.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C():
			if n, err := s.SweepOnce(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				s.log.Error("heartbeat sweep failed", "error", err)
			} else if n > 0 {
				s.log.Warn("heartbeats went missing", "count", n)
			}
		}
	}
}

// SweepOnce checks every switch and raises an alert for each newly missing
// one. It returns how many went missing.
//
// Exported so a caller can sweep synchronously, and so tests can assert one
// sweep at a time against a fake clock.
func (s *Sweeper) SweepOnce(ctx context.Context) (int, error) {
	now := s.clk.Now()

	overdue, err := s.db.OverdueHeartbeats(ctx, now)
	if err != nil {
		return 0, err
	}

	var raised int
	for _, h := range overdue {
		// Flip the state first. Only the transition raises an alert, so a
		// switch that stays down produces one incident rather than one per
		// sweep for as long as it is broken.
		changed, err := markMissing(ctx, s.db, h.ID)
		if err != nil {
			return raised, err
		}
		if !changed {
			continue
		}

		s.log.Error("heartbeat missed its window",
			"heartbeat", h.Name, "team", h.Team,
			"expected_interval", h.ExpectedInterval, "grace_period", h.GracePeriod,
			"last_ping", h.LastPingAt)

		if s.sink != nil {
			if _, err := s.sink.Ingest(ctx, []core.Alert{syntheticAlert(h, now)}); err != nil {
				return raised, fmt.Errorf("raise alert for heartbeat %q: %w", h.Name, err)
			}
		}
		raised++
	}
	return raised, nil
}

func markMissing(ctx context.Context, db *store.DB, id int64) (bool, error) {
	var changed bool
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		changed, err = store.MarkHeartbeatMissing(ctx, tx, id)
		return err
	})
	return changed, err
}

// syntheticAlert turns a missed heartbeat into an ordinary alert.
//
// The labels are chosen so an operator can route heartbeats exactly like any
// other alert: match on alertname, or on the heartbeat's own name, or on the
// team it belongs to.
func syntheticAlert(h core.Heartbeat, now time.Time) core.Alert {
	last := "never"
	if h.LastPingAt != nil {
		last = h.LastPingAt.Format(time.RFC3339)
	}
	return core.Alert{
		Source: core.SourceHeartbeat,
		Status: core.AlertFiring,
		Labels: core.Labels{
			"alertname": "HeartbeatMissing",
			"heartbeat": h.Name,
			"team":      h.Team,
			"severity":  string(h.Severity),
		},
		Annotations: core.Annotations{
			"summary": fmt.Sprintf("Heartbeat %q has stopped reporting", h.Name),
			"description": fmt.Sprintf(
				"Expected a ping every %s with %s of grace. Last ping: %s.",
				h.ExpectedInterval, h.GracePeriod, last),
		},
		StartsAt:   now,
		ReceivedAt: now,
	}
}

// Sync ensures every heartbeat declared in configuration exists in the
// database, minting a token for any that is new.
//
// The token is generated rather than configured so a secret that could keep a
// dead job looking alive never has to live in a git repository. It is returned
// for the newly created ones only, because that is the one moment it can be
// shown: it is not recoverable later by design.
func Sync(ctx context.Context, db *store.DB, declared []DeclaredHeartbeat, now time.Time) (map[string]string, error) {
	existing, err := db.Heartbeats(ctx)
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(existing))
	for _, h := range existing {
		known[h.Name] = true
	}

	created := map[string]string{}
	for _, d := range declared {
		if known[d.Name] {
			continue
		}
		token, err := store.NewHeartbeatToken()
		if err != nil {
			return nil, err
		}
		err = db.Tx(ctx, func(tx *sql.Tx) error {
			_, err := store.InsertHeartbeat(ctx, tx, core.Heartbeat{
				Name:             d.Name,
				Token:            token,
				ExpectedInterval: d.ExpectedInterval,
				GracePeriod:      d.GracePeriod,
				Team:             d.Team,
				Severity:         d.Severity,
				State:            core.HeartbeatNeverSeen,
				CreatedAt:        now,
			})
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("register heartbeat %q: %w", d.Name, err)
		}
		created[d.Name] = token
	}
	return created, nil
}

// DeclaredHeartbeat is a heartbeat from configuration, converted away from the
// config types so this package does not depend on them.
type DeclaredHeartbeat struct {
	Name             string
	ExpectedInterval time.Duration
	GracePeriod      time.Duration
	Team             string
	Severity         core.Severity
}
