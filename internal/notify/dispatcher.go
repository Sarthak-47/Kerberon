package notify

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/store"
)

// DefaultLease is how long a notification may sit in sending before another
// worker reclaims it. A worker that died mid-send leaves a row in that state,
// and reclaiming it is what stops a page disappearing with the process.
const DefaultLease = 2 * time.Minute

// Dispatcher drains the outbox.
type Dispatcher struct {
	db       *store.DB
	clk      clock.Clock
	channels map[core.Channel]Channel
	breaker  *Breaker
	log      *slog.Logger

	workers     int
	pollEvery   time.Duration
	lease       time.Duration
	maxAttempts int
	jitter      func() float64

	// OnDeadLetter is called when a page could not be delivered at all.
	// Failing to page is itself a critical condition and must be surfaced,
	// never silently dropped (spec section 8.3).
	onDeadLetter DeadLetterFunc

	wake chan struct{}
}

// DeadLetterFunc handles a notification that exhausted every attempt. It runs
// inside a transaction and may only write to the database.
type DeadLetterFunc func(ctx context.Context, tx *sql.Tx, n core.Notification) error

// Options configures a Dispatcher.
type Options struct {
	Workers     int
	PollEvery   time.Duration
	Lease       time.Duration
	MaxAttempts int
	// Jitter returns a value in [0,1). Defaults to math/rand. Tests inject a
	// deterministic function so the backoff schedule can be asserted exactly.
	Jitter       func() float64
	OnDeadLetter DeadLetterFunc
	Breaker      *Breaker
	Logger       *slog.Logger
}

func (o Options) withDefaults(clk clock.Clock) Options {
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if o.PollEvery <= 0 {
		o.PollEvery = time.Second
	}
	if o.Lease <= 0 {
		o.Lease = DefaultLease
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = MaxAttempts
	}
	if o.Jitter == nil {
		// Seeded per dispatcher rather than globally so two instances do not
		// retry in lockstep.
		rng := rand.New(rand.NewSource(time.Now().UnixNano())) //kerberon:allow-clock -- seeding entropy, not measuring time
		var mu sync.Mutex
		o.Jitter = func() float64 {
			mu.Lock()
			defer mu.Unlock()
			return rng.Float64()
		}
	}
	if o.Breaker == nil {
		o.Breaker = NewBreaker(clk, BreakerOptions{})
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// New creates a Dispatcher over the given channels.
func New(db *store.DB, clk clock.Clock, channels []Channel, opts Options) *Dispatcher {
	opts = opts.withDefaults(clk)

	byName := make(map[core.Channel]Channel, len(channels))
	for _, c := range channels {
		byName[c.Name()] = c
	}

	return &Dispatcher{
		db:           db,
		clk:          clk,
		channels:     byName,
		breaker:      opts.Breaker,
		log:          opts.Logger,
		workers:      opts.Workers,
		pollEvery:    opts.PollEvery,
		lease:        opts.Lease,
		maxAttempts:  opts.MaxAttempts,
		jitter:       opts.Jitter,
		onDeadLetter: opts.OnDeadLetter,
		wake:         make(chan struct{}, 1),
	}
}

// Breaker exposes the circuit breaker, for the UI and for tests.
func (d *Dispatcher) Breaker() *Breaker { return d.breaker }

// Wake asks the dispatcher to poll immediately. Call it after committing a
// transaction that enqueued notifications.
func (d *Dispatcher) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run drains the outbox until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) error {
	d.log.Info("notification dispatcher started", "workers", d.workers)
	defer d.log.Info("notification dispatcher stopped")

	for {
		if ctx.Err() != nil {
			return nil
		}

		n, err := d.DrainOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			d.log.Error("dispatcher poll failed", "error", err)
		}
		if n > 0 {
			// There may be more waiting; do not sleep first.
			continue
		}

		sleep := d.clk.NewTimer(d.pollEvery)
		select {
		case <-ctx.Done():
			sleep.Stop()
			return nil
		case <-d.wake:
			sleep.Stop()
		case <-sleep.C():
		}
	}
}

// DrainOnce claims one batch and delivers it, returning how many were
// attempted. Run calls it in a loop; it is exported so a caller can flush the
// outbox synchronously, which is also how the tests assert one attempt at a
// time against a fake clock.
func (d *Dispatcher) DrainOnce(ctx context.Context) (int, error) {
	batch, err := d.db.ClaimDueNotifications(ctx, d.clk.Now(), d.lease, d.workers)
	if err != nil {
		return 0, err
	}
	if len(batch) == 0 {
		return 0, nil
	}

	var wg sync.WaitGroup
	for i := range batch {
		wg.Add(1)
		go func(n core.Notification) {
			defer wg.Done()
			d.deliver(ctx, n)
		}(batch[i])
	}
	wg.Wait()
	return len(batch), nil
}

// deliver attempts one notification and records the outcome.
func (d *Dispatcher) deliver(ctx context.Context, n core.Notification) {
	ch, known := d.channels[n.Channel]
	if !known {
		// Configured to use a channel this build cannot deliver on. Retrying
		// cannot help, so fail it now and let dead-lettering find another way.
		d.fail(ctx, n, fmt.Errorf("no channel registered for %q", n.Channel), false)
		return
	}

	if !d.breaker.Allow(n.Channel) {
		// The channel is out of service. Fail fast rather than waiting on it,
		// so the incident can be pursued by other means.
		d.fail(ctx, n, fmt.Errorf("channel %q circuit breaker is open", n.Channel), true)
		return
	}

	err := ch.Send(ctx, Message{
		IncidentID:  n.IncidentID,
		Destination: n.Destination,
		Title:       n.Title,
		Body:        n.Body,
		Severity:    n.Severity,
		AckLink:     n.AckURL,
	})
	if err == nil {
		d.breaker.Succeed(n.Channel)
		if err := d.db.Tx(ctx, func(tx *sql.Tx) error {
			return store.MarkNotificationSent(ctx, tx, n.ID, d.clk.Now())
		}); err != nil {
			// The page went out; only the bookkeeping failed. Say so plainly,
			// because the row will be retried and the human may see it twice.
			d.log.Error("delivered a page but could not record it; it may be retried",
				"notification_id", n.ID, "channel", n.Channel, "error", err)
		}
		d.log.Info("page delivered",
			"notification_id", n.ID, "incident_id", n.IncidentID,
			"channel", n.Channel, "user", n.TargetUser, "attempt", n.Attempts)
		return
	}

	if opened := d.breaker.Fail(n.Channel); opened {
		d.log.Warn("channel circuit breaker opened; pages will fail over",
			"channel", n.Channel, "consecutive_failures", d.breaker.Failures(n.Channel))
	}
	d.fail(ctx, n, err, IsRetryable(err))
}

// fail records a failed attempt, retrying or dead-lettering as appropriate.
func (d *Dispatcher) fail(ctx context.Context, n core.Notification, cause error, retryable bool) {
	exhausted := n.Attempts >= d.maxAttempts

	if retryable && !exhausted {
		retryAt := d.clk.Now().Add(Backoff(n.Attempts, d.jitter))
		if err := d.db.Tx(ctx, func(tx *sql.Tx) error {
			return store.MarkNotificationFailed(ctx, tx, n.ID, retryAt, cause.Error())
		}); err != nil {
			d.log.Error("could not schedule a retry", "notification_id", n.ID, "error", err)
			return
		}
		d.log.Warn("page delivery failed; will retry",
			"notification_id", n.ID, "channel", n.Channel, "attempt", n.Attempts,
			"retry_at", retryAt, "error", cause)
		return
	}

	// Out of road. The page did not reach anyone.
	err := d.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := store.MarkNotificationDead(ctx, tx, n.ID, cause.Error()); err != nil {
			return err
		}
		if d.onDeadLetter != nil {
			return d.onDeadLetter(ctx, tx, n)
		}
		return nil
	})
	if err != nil {
		d.log.Error("could not dead-letter a notification", "notification_id", n.ID, "error", err)
		return
	}

	reason := "delivery failed permanently"
	if !retryable {
		reason = "delivery failed and retrying cannot help"
	} else if exhausted {
		reason = fmt.Sprintf("delivery failed after %d attempts", n.Attempts)
	}
	// This is the paging system failing to page. It must never be quiet.
	d.log.Error("PAGE NOT DELIVERED: "+reason,
		"notification_id", n.ID, "incident_id", n.IncidentID,
		"channel", n.Channel, "user", n.TargetUser, "error", cause)
}

// ErrNoChannel reports that no registered channel can reach a target.
var ErrNoChannel = errors.New("no usable channel for target")

// Available returns the registered channels whose breakers are closed, in the
// order given, so a caller can fail over to the next one.
func (d *Dispatcher) Available(preferred []core.Channel) []core.Channel {
	var out []core.Channel
	for _, name := range preferred {
		if _, ok := d.channels[name]; !ok {
			continue
		}
		if !d.breaker.Allow(name) {
			continue
		}
		out = append(out, name)
	}
	return out
}
