// Package timer is Kerberon's durable scheduler.
//
// Everything Kerberon promises depends on doing something later: "if nobody
// acknowledges within five minutes, page the secondary." An in-memory
// time.AfterFunc loses that promise on restart, which is unacceptable because
// process restarts correlate with exactly the incidents that need paging. So
// every future action is a row in the timers table.
//
// # Exactly once
//
// A timer's effect must be a pure database state change. Advancing an
// incident, enqueueing notifications into the outbox, scheduling the next
// timer — all writes. Nothing with an external side effect may run in a
// handler; that is what the notification outbox and its dispatch workers are
// for.
//
// Because the effect is only writes, the whole tick is one transaction: load
// the timer and confirm it is pending, apply the effect, mark it complete,
// commit. Exactly-once execution then follows from SQLite's atomicity rather
// than from application logic. A crash mid-transaction rolls everything back
// and the timer is simply still pending on restart.
//
// The spec's original design claimed a timer in one transaction and completed
// it in another. A crash between the two would leave claimed_at set with
// completed_at null; the recovery pass would re-select the timer, the claim
// would match no rows, and the timer would be skipped forever — a silently
// missed page. See docs/DECISIONS.md D1. The claimed_at column is retained,
// unused, for the lease-based claim that leader election would need.
//
// # Crash recovery
//
// There is no separate recovery path. The scheduler always asks for the
// earliest pending timer, and after a restart that query naturally returns
// timers whose fire_at is already past, in fire_at order. A process that was
// down for ten minutes works through the backlog oldest first.
package timer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/store"
)

// Handler executes a due timer's effect.
//
// It receives the transaction that will also mark the timer complete, and it
// must do all its work on that transaction. A handler may not perform I/O:
// no HTTP calls, no sending notifications. Enqueue to the outbox instead.
type Handler interface {
	HandleTimer(ctx context.Context, tx *sql.Tx, t core.Timer) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, tx *sql.Tx, t core.Timer) error

func (f HandlerFunc) HandleTimer(ctx context.Context, tx *sql.Tx, t core.Timer) error {
	return f(ctx, tx, t)
}

// Options tunes a Scheduler. The zero value is usable.
type Options struct {
	// MaxIdleWait caps how long the loop sleeps when nothing is scheduled.
	// Wake() handles the normal case; this only bounds the damage if a wake is
	// ever missed. Defaults to one minute.
	MaxIdleWait time.Duration
	// RetryBackoff is how long a timer is skipped after its handler fails,
	// doubling up to MaxRetryBackoff. Defaults to one second.
	RetryBackoff time.Duration
	// MaxRetryBackoff caps that growth. Defaults to one minute.
	MaxRetryBackoff time.Duration
	Logger          *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.MaxIdleWait <= 0 {
		o.MaxIdleWait = time.Minute
	}
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = time.Second
	}
	if o.MaxRetryBackoff <= 0 {
		o.MaxRetryBackoff = time.Minute
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Scheduler runs due timers. Exactly one goroutine executes timers; Run must
// not be called concurrently with itself on the same database.
type Scheduler struct {
	db   *store.DB
	clk  clock.Clock
	opts Options
	log  *slog.Logger
	wake chan struct{}

	mu       sync.RWMutex
	handlers map[core.TimerKind]Handler

	// retryAfter defers timers whose handler failed, so one bad timer cannot
	// spin the loop. In-memory only: a restart retries immediately, which is
	// the right behaviour for a transient fault.
	retryMu    sync.Mutex
	retryAfter map[int64]time.Time
	retryDelay map[int64]time.Duration
}

// New creates a Scheduler. Register handlers before calling Run.
func New(db *store.DB, clk clock.Clock, opts Options) *Scheduler {
	opts = opts.withDefaults()
	return &Scheduler{
		db:         db,
		clk:        clk,
		opts:       opts,
		log:        opts.Logger,
		wake:       make(chan struct{}, 1),
		handlers:   make(map[core.TimerKind]Handler),
		retryAfter: make(map[int64]time.Time),
		retryDelay: make(map[int64]time.Duration),
	}
}

// Register installs the handler for a timer kind. It panics on a duplicate or
// invalid kind, both of which are programming errors caught at startup.
func (s *Scheduler) Register(kind core.TimerKind, h Handler) {
	if !kind.Valid() {
		panic(fmt.Sprintf("timer: invalid kind %q", kind))
	}
	if h == nil {
		panic(fmt.Sprintf("timer: nil handler for kind %q", kind))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.handlers[kind]; dup {
		panic(fmt.Sprintf("timer: handler for kind %q registered twice", kind))
	}
	s.handlers[kind] = h
}

func (s *Scheduler) handlerFor(kind core.TimerKind) (Handler, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.handlers[kind]
	return h, ok
}

// Wake nudges the loop to re-read the earliest pending timer. Call it after
// committing a transaction that scheduled or cancelled a timer; before the
// commit the scheduler would not see the row.
//
// It never blocks. A pending wake already covers any number of changes.
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Schedule inserts a timer on tx. The caller must Wake after committing.
func (s *Scheduler) Schedule(ctx context.Context, tx *sql.Tx, t core.Timer) (int64, error) {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = s.clk.Now()
	}
	return store.InsertTimer(ctx, tx, t)
}

// ScheduleNow inserts a timer in its own transaction and wakes the loop. Use it
// from paths that are not already inside a transaction, such as ingest opening
// a group_wait window.
func (s *Scheduler) ScheduleNow(ctx context.Context, t core.Timer) (int64, error) {
	var id int64
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = s.Schedule(ctx, tx, t)
		return err
	})
	if err != nil {
		return 0, err
	}
	s.Wake()
	return id, nil
}

// Cancel stops a pending timer and wakes the loop.
func (s *Scheduler) Cancel(ctx context.Context, id int64) error {
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		return store.CancelTimer(ctx, tx, id, s.clk.Now())
	})
	if err != nil {
		return err
	}
	s.Wake()
	return nil
}

// CancelForIncident cancels every pending timer for an incident, optionally
// restricted to certain kinds. An acknowledgement uses this to stop escalation.
func (s *Scheduler) CancelForIncident(ctx context.Context, incidentID int64, kinds ...core.TimerKind) (int64, error) {
	var n int64
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = store.CancelIncidentTimers(ctx, tx, incidentID, s.clk.Now(), kinds...)
		return err
	})
	if err != nil {
		return 0, err
	}
	s.Wake()
	return n, nil
}

// Run executes due timers until ctx is cancelled. It returns nil on a clean
// shutdown.
func (s *Scheduler) Run(ctx context.Context) error {
	s.log.Info("timer scheduler started")
	defer s.log.Info("timer scheduler stopped")

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		wait, err := s.step(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// A database-level failure. Pause briefly rather than spinning.
			s.log.Error("timer scheduler step failed", "error", err)
			wait = s.opts.RetryBackoff
		}
		if wait <= 0 {
			// A timer was executed; look for the next one immediately.
			continue
		}
		if wait > s.opts.MaxIdleWait {
			wait = s.opts.MaxIdleWait
		}
		// NewTimer rather than After: when the wake channel or ctx wins the
		// select, Stop releases the waiter. After would leave it registered
		// until it eventually fired, leaking one per loop iteration.
		sleep := s.clk.NewTimer(wait)
		select {
		case <-ctx.Done():
			sleep.Stop()
			return nil
		case <-s.wake:
			sleep.Stop()
		case <-sleep.C():
		}
	}
}

// batchSize is how many upcoming timers step considers. A timer backing off
// after a failure must not block the ones behind it, so the scheduler looks at
// a small window rather than only the single earliest row.
const batchSize = 16

// step processes at most one timer. It returns how long to wait before looking
// again; zero means "there may be more work right now".
func (s *Scheduler) step(ctx context.Context) (time.Duration, error) {
	batch, err := s.db.NextPendingTimers(ctx, batchSize)
	if err != nil {
		return 0, err
	}
	if len(batch) == 0 {
		return s.opts.MaxIdleWait, nil
	}

	now := s.clk.Now()
	wait := s.opts.MaxIdleWait

	for _, t := range batch {
		// Skip timers that are backing off, remembering the soonest retry so
		// the loop wakes for it.
		if until, deferred := s.deferredUntil(t.ID); deferred && until.After(now) {
			if d := until.Sub(now); d < wait {
				wait = d
			}
			continue
		}
		if t.FireAt.After(now) {
			// The batch is ordered by fire_at, so everything after this is
			// later still.
			if d := t.FireAt.Sub(now); d < wait {
				wait = d
			}
			break
		}
		if err := s.execute(ctx, t); err != nil {
			return 0, err
		}
		return 0, nil
	}
	return wait, nil
}

// execute runs one timer's effect. Everything happens in a single transaction:
// the pending check, the effect, and marking the timer complete.
func (s *Scheduler) execute(ctx context.Context, t core.Timer) error {
	handler, ok := s.handlerFor(t.Kind)
	if !ok {
		// A timer of a kind this build does not know — most plausibly a
		// downgrade. Defer rather than complete it: leaving the row pending
		// preserves the escalation for an upgraded binary, whereas marking it
		// done would silently drop a page.
		s.log.Error("no handler registered for timer kind; deferring",
			"timer_id", t.ID, "kind", t.Kind, "incident_id", t.IncidentID)
		s.defer_(t.ID)
		return nil
	}

	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		// Re-read inside the transaction. An acknowledgement may have
		// cancelled this timer since it was selected.
		fresh, err := store.LoadPendingTimer(ctx, tx, t.ID)
		if err != nil {
			return err
		}
		if err := handler.HandleTimer(ctx, tx, fresh); err != nil {
			return fmt.Errorf("timer %d (%s): %w", fresh.ID, fresh.Kind, err)
		}
		return store.CompleteTimer(ctx, tx, fresh.ID, s.clk.Now())
	})

	switch {
	case err == nil:
		s.clearDefer(t.ID)
		s.log.Debug("timer fired", "timer_id", t.ID, "kind", t.Kind,
			"incident_id", t.IncidentID)
		return nil

	case errors.Is(err, store.ErrTimerNotPending):
		// Cancelled or completed between selection and execution. Normal.
		s.clearDefer(t.ID)
		s.log.Debug("timer no longer pending; skipping", "timer_id", t.ID)
		return nil

	default:
		// The transaction rolled back, so the effect did not happen and the
		// timer is still pending. Defer and retry.
		s.log.Error("timer handler failed; will retry",
			"timer_id", t.ID, "kind", t.Kind, "error", err)
		s.defer_(t.ID)
		return nil
	}
}

// ─── Retry bookkeeping ────────────────────────────────────────────────────

func (s *Scheduler) deferredUntil(id int64) (time.Time, bool) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	until, ok := s.retryAfter[id]
	return until, ok
}

// defer_ backs a timer off with doubling delay. Named with a trailing
// underscore because "defer" is a keyword.
func (s *Scheduler) defer_(id int64) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()

	delay := s.retryDelay[id]
	if delay == 0 {
		delay = s.opts.RetryBackoff
	} else {
		delay *= 2
		if delay > s.opts.MaxRetryBackoff {
			delay = s.opts.MaxRetryBackoff
		}
	}
	s.retryDelay[id] = delay
	s.retryAfter[id] = s.clk.Now().Add(delay)
}

func (s *Scheduler) clearDefer(id int64) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	delete(s.retryAfter, id)
	delete(s.retryDelay, id)
}
