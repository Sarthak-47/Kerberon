package notify_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/notify"
	"github.com/Sarthak-47/kerberon/internal/store"
)

const t0RFC = "2026-08-15T09:00:00Z"

// ─── Fake channel ─────────────────────────────────────────────────────────

// fakeChannel fails deterministically so the retry schedule, the breaker and
// dead-lettering can all be asserted rather than observed.
type fakeChannel struct {
	name core.Channel

	mu       sync.Mutex
	sent     []notify.Message
	failWith error
	// failFirst makes the first N attempts fail, then succeed. Zero means the
	// failWith behaviour applies to every attempt.
	failFirst int
	attempts  int
}

func newFake(name core.Channel) *fakeChannel {
	return &fakeChannel{name: name}
}

func (f *fakeChannel) Name() core.Channel { return f.name }

func (f *fakeChannel) Capabilities() notify.Capabilities {
	return notify.Capabilities{IsLoud: true}
}

func (f *fakeChannel) Send(_ context.Context, m notify.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++

	if f.failWith != nil && (f.failFirst == 0 || f.attempts <= f.failFirst) {
		return f.failWith
	}
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeChannel) delivered() []notify.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notify.Message(nil), f.sent...)
}

func (f *fakeChannel) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// ─── Harness ──────────────────────────────────────────────────────────────

type harness struct {
	t          *testing.T
	db         *store.DB
	clk        *clock.FakeClock
	incidentID int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	root := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create .tmp: %v", err)
	}
	dir, err := os.MkdirTemp(root, "notify-test-")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(dir, "kerberon.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	h := &harness{t: t, db: db, clk: clock.NewFakeAt(t0RFC)}
	err = db.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`
			INSERT INTO incidents
				(group_key, team, policy, severity, title, status, created_at, last_alert_at)
			VALUES ('g', 'platform', 'p', 'critical', 'test', 'triggered', 1000, 1000)`)
		if err != nil {
			return err
		}
		h.incidentID, err = res.LastInsertId()
		return err
	})
	if err != nil {
		t.Fatalf("seed incident: %v", err)
	}
	return h
}

// enqueue adds one outbox row.
func (h *harness) enqueue(ch core.Channel, user string, step int) int64 {
	h.t.Helper()
	ctx := context.Background()

	var id int64
	err := h.db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = store.EnqueueNotification(ctx, tx, core.Notification{
			IncidentID:     h.incidentID,
			IdempotencyKey: store.IdempotencyKey(h.incidentID, step, user, ch, 0),
			StepIndex:      step,
			TargetUser:     user,
			Channel:        ch,
			Destination:    "dest-" + user,
			Body:           "the api is down",
			State:          core.NotifPending,
			CreatedAt:      h.clk.Now(),
		})
		return err
	})
	if err != nil {
		h.t.Fatalf("enqueue: %v", err)
	}
	return id
}

func (h *harness) notification(id int64) core.Notification {
	h.t.Helper()
	n, err := h.db.Notification(context.Background(), id)
	if err != nil {
		h.t.Fatalf("read notification %d: %v", id, err)
	}
	return n
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fixedJitter makes the backoff schedule exactly assertable.
func fixedJitter(v float64) func() float64 { return func() float64 { return v } }

// ─── Delivery ─────────────────────────────────────────────────────────────

func TestSuccessfulDelivery(t *testing.T) {
	h := newHarness(t)
	ch := newFake(core.ChannelNtfy)
	d := notify.New(h.db, h.clk, []notify.Channel{ch}, notify.Options{Logger: quietLogger()})

	id := h.enqueue(core.ChannelNtfy, "sarthak", 0)
	if _, err := d.DrainOnce(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	msgs := ch.delivered()
	if len(msgs) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(msgs))
	}
	if msgs[0].Destination != "dest-sarthak" || msgs[0].Body != "the api is down" {
		t.Errorf("message = %+v", msgs[0])
	}

	n := h.notification(id)
	if n.State != core.NotifSent {
		t.Errorf("state = %q, want sent", n.State)
	}
	if n.SentAt == nil {
		t.Error("sent_at not recorded")
	}
	if n.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", n.Attempts)
	}
}

// The idempotency key is what makes "exactly once from the human's
// perspective" achievable on at-least-once machinery.
func TestDuplicateEnqueueIsRejected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	n := core.Notification{
		IncidentID:     h.incidentID,
		IdempotencyKey: store.IdempotencyKey(h.incidentID, 0, "sarthak", core.ChannelNtfy, 0),
		StepIndex:      0,
		TargetUser:     "sarthak",
		Channel:        core.ChannelNtfy,
		Destination:    "dest",
		Body:           "b",
		State:          core.NotifPending,
		CreatedAt:      h.clk.Now(),
	}

	if err := h.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.EnqueueNotification(ctx, tx, n)
		return err
	}); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}

	err := h.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.EnqueueNotification(ctx, tx, n)
		return err
	})
	if !errors.Is(err, store.ErrDuplicateNotification) {
		t.Fatalf("second enqueue returned %v, want ErrDuplicateNotification", err)
	}
}

// A deliberate re-page on a repeating policy is a different notification from
// a retry of the same one.
func TestAttemptGroupDistinguishesARePage(t *testing.T) {
	first := store.IdempotencyKey(1, 0, "sarthak", core.ChannelNtfy, 0)
	second := store.IdempotencyKey(1, 0, "sarthak", core.ChannelNtfy, 1)
	if first == second {
		t.Error("a second pass of a repeating policy collides with the first")
	}
}

func TestIdempotencyKeyBindsEveryField(t *testing.T) {
	base := store.IdempotencyKey(1, 0, "sarthak", core.ChannelNtfy, 0)
	cases := map[string]string{
		"incident": store.IdempotencyKey(2, 0, "sarthak", core.ChannelNtfy, 0),
		"step":     store.IdempotencyKey(1, 1, "sarthak", core.ChannelNtfy, 0),
		"user":     store.IdempotencyKey(1, 0, "priya", core.ChannelNtfy, 0),
		"channel":  store.IdempotencyKey(1, 0, "sarthak", core.ChannelTelegram, 0),
	}
	for name, got := range cases {
		if got == base {
			t.Errorf("changing the %s did not change the key", name)
		}
	}
	// Length prefixing: "ab"+step0 must not collide with "a"+something.
	if store.IdempotencyKey(1, 0, "ab", core.ChannelNtfy, 0) ==
		store.IdempotencyKey(1, 0, "a", core.ChannelNtfy, 0) {
		t.Error("user ids collide across the field boundary")
	}
}

// ─── Retry and backoff ────────────────────────────────────────────────────

func TestRetryableFailureIsRescheduledWithBackoff(t *testing.T) {
	h := newHarness(t)
	ch := newFake(core.ChannelNtfy)
	ch.failWith = notify.Retryable(errors.New("connection reset"))

	// No jitter, so the schedule is exactly the documented one.
	d := notify.New(h.db, h.clk, []notify.Channel{ch}, notify.Options{
		Logger: quietLogger(),
		Jitter: fixedJitter(1.0),
	})

	id := h.enqueue(core.ChannelNtfy, "sarthak", 0)

	// Spec section 8.3: 5s, 15s, 45s, 2m, 5m.
	want := []time.Duration{
		5 * time.Second, 15 * time.Second, 45 * time.Second,
		2 * time.Minute, 5 * time.Minute,
	}
	for i, wantDelay := range want {
		if _, err := d.DrainOnce(context.Background()); err != nil {
			t.Fatalf("drain %d: %v", i, err)
		}
		n := h.notification(id)
		if i == len(want)-1 {
			// The final attempt exhausts the budget and dead-letters.
			if n.State != core.NotifDead {
				t.Errorf("after the last attempt state = %q, want dead", n.State)
			}
			break
		}
		if n.State != core.NotifFailed {
			t.Fatalf("attempt %d: state = %q, want failed", i+1, n.State)
		}
		if n.NextAttemptAt == nil {
			t.Fatalf("attempt %d: no retry scheduled", i+1)
		}
		if got := n.NextAttemptAt.Sub(h.clk.Now()); got != wantDelay {
			t.Errorf("attempt %d: retry in %v, want %v", i+1, got, wantDelay)
		}
		h.clk.Advance(wantDelay)
	}
}

// Jitter spreads retries so a recovering provider is not hit by every queued
// page at the same instant.
func TestBackoffAppliesFullJitter(t *testing.T) {
	// With jitter at zero the delay collapses to zero; at one it is the full
	// step. Both ends of the range must be reachable.
	if got := notify.Backoff(1, fixedJitter(0)); got != 0 {
		t.Errorf("jitter 0 gave %v, want 0", got)
	}
	if got := notify.Backoff(1, fixedJitter(1)); got != 5*time.Second {
		t.Errorf("jitter 1 gave %v, want the full 5s step", got)
	}
	if got := notify.Backoff(1, fixedJitter(0.5)); got != 2500*time.Millisecond {
		t.Errorf("jitter 0.5 gave %v, want half the step", got)
	}
	// Past the end of the schedule the last step repeats rather than growing
	// without bound.
	if got := notify.Backoff(99, fixedJitter(1)); got != 5*time.Minute {
		t.Errorf("beyond the schedule gave %v, want the final 5m step", got)
	}
}

// Retrying a failure that cannot succeed wastes the window in which someone
// could still have been paged another way.
func TestUnretryableFailureDeadLettersImmediately(t *testing.T) {
	h := newHarness(t)
	ch := newFake(core.ChannelNtfy)
	ch.failWith = errors.New("destination is not a valid URL")

	d := notify.New(h.db, h.clk, []notify.Channel{ch}, notify.Options{Logger: quietLogger()})
	id := h.enqueue(core.ChannelNtfy, "sarthak", 0)

	if _, err := d.DrainOnce(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	n := h.notification(id)
	if n.State != core.NotifDead {
		t.Errorf("state = %q, want dead on the first unretryable failure", n.State)
	}
	if n.Attempts != 1 {
		t.Errorf("attempts = %d, want 1; it should not have been retried", n.Attempts)
	}
}

func TestRecoveryAfterTransientFailures(t *testing.T) {
	h := newHarness(t)
	ch := newFake(core.ChannelNtfy)
	ch.failWith = notify.Retryable(errors.New("temporary"))
	ch.failFirst = 2

	d := notify.New(h.db, h.clk, []notify.Channel{ch}, notify.Options{
		Logger: quietLogger(),
		Jitter: fixedJitter(1.0),
	})
	id := h.enqueue(core.ChannelNtfy, "sarthak", 0)

	for i := 0; i < 3; i++ {
		if _, err := d.DrainOnce(context.Background()); err != nil {
			t.Fatalf("drain %d: %v", i, err)
		}
		h.clk.Advance(time.Minute)
	}

	if n := h.notification(id); n.State != core.NotifSent {
		t.Errorf("state = %q, want sent once the provider recovered", n.State)
	}
	if len(ch.delivered()) != 1 {
		t.Errorf("delivered %d times, want exactly 1", len(ch.delivered()))
	}
}

// ─── Dead lettering ───────────────────────────────────────────────────────

// "The paging system failed to page" must be a detected, surfaced state.
func TestDeadLetterHookIsCalled(t *testing.T) {
	h := newHarness(t)
	ch := newFake(core.ChannelNtfy)
	ch.failWith = errors.New("permanent")

	var called atomic.Int64
	var got core.Notification
	var mu sync.Mutex

	d := notify.New(h.db, h.clk, []notify.Channel{ch}, notify.Options{
		Logger: quietLogger(),
		OnDeadLetter: func(ctx context.Context, tx *sql.Tx, n core.Notification) error {
			called.Add(1)
			mu.Lock()
			got = n
			mu.Unlock()
			// A dead letter handler may only write to the database, like any
			// other effect that must commit atomically.
			return store.InsertEvent(ctx, tx, n.IncidentID, core.EventNotified,
				`{"dead_letter":true}`, h.clk.Now())
		},
	})

	h.enqueue(core.ChannelNtfy, "sarthak", 0)
	if _, err := d.DrainOnce(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if called.Load() != 1 {
		t.Fatalf("dead letter hook called %d times, want 1", called.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if got.TargetUser != "sarthak" {
		t.Errorf("hook received %+v", got)
	}

	// The hook's write committed with the state change.
	events, err := h.db.Events(context.Background(), h.incidentID)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) == 0 {
		t.Error("the dead letter hook's event was not committed")
	}
}

// ─── Circuit breaker ──────────────────────────────────────────────────────

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	clk := clock.NewFakeAt(t0RFC)
	b := notify.NewBreaker(clk, notify.BreakerOptions{Threshold: 3, Cooldown: time.Minute})

	for i := 0; i < 2; i++ {
		if opened := b.Fail(core.ChannelNtfy); opened {
			t.Fatalf("breaker opened after %d failures, want 3", i+1)
		}
		if !b.Allow(core.ChannelNtfy) {
			t.Fatalf("breaker blocked after %d failures", i+1)
		}
	}
	if opened := b.Fail(core.ChannelNtfy); !opened {
		t.Fatal("breaker did not open at the threshold")
	}
	if b.Allow(core.ChannelNtfy) {
		t.Error("an open breaker still allows sends")
	}

	// Another channel is unaffected: that is the whole point.
	if !b.Allow(core.ChannelTelegram) {
		t.Error("opening one channel's breaker blocked another")
	}
}

// After the cooldown a single probe is admitted, so a recovered provider comes
// back into service without anyone intervening.
func TestBreakerRecoversAfterCooldown(t *testing.T) {
	clk := clock.NewFakeAt(t0RFC)
	b := notify.NewBreaker(clk, notify.BreakerOptions{Threshold: 1, Cooldown: time.Minute})

	b.Fail(core.ChannelNtfy)
	if b.Allow(core.ChannelNtfy) {
		t.Fatal("breaker should be open")
	}

	clk.Advance(59 * time.Second)
	if b.Allow(core.ChannelNtfy) {
		t.Error("breaker reopened before the cooldown elapsed")
	}

	clk.Advance(time.Second)
	if !b.Allow(core.ChannelNtfy) {
		t.Error("breaker did not admit a probe after the cooldown")
	}

	b.Succeed(core.ChannelNtfy)
	if !b.Allow(core.ChannelNtfy) || b.Failures(core.ChannelNtfy) != 0 {
		t.Error("a success should close the breaker and reset the count")
	}
}

// If a provider is down the page must fail fast rather than queue behind it.
func TestOpenBreakerFailsSendsFast(t *testing.T) {
	h := newHarness(t)
	ch := newFake(core.ChannelNtfy)
	ch.failWith = notify.Retryable(errors.New("down"))

	b := notify.NewBreaker(h.clk, notify.BreakerOptions{Threshold: 1, Cooldown: time.Hour})
	d := notify.New(h.db, h.clk, []notify.Channel{ch}, notify.Options{
		Logger:  quietLogger(),
		Breaker: b,
		Jitter:  fixedJitter(1.0),
	})

	h.enqueue(core.ChannelNtfy, "sarthak", 0)
	if _, err := d.DrainOnce(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !b.Open(core.ChannelNtfy) {
		t.Fatal("breaker did not open")
	}
	attemptsBefore := ch.attemptCount()

	// A second page on the same channel must not reach the provider at all.
	h.enqueue(core.ChannelNtfy, "priya", 0)
	h.clk.Advance(10 * time.Second)
	if _, err := d.DrainOnce(context.Background()); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if ch.attemptCount() != attemptsBefore {
		t.Errorf("the provider was contacted despite an open breaker (%d then %d attempts)",
			attemptsBefore, ch.attemptCount())
	}
}

func TestAvailableSkipsOpenChannels(t *testing.T) {
	h := newHarness(t)
	b := notify.NewBreaker(h.clk, notify.BreakerOptions{Threshold: 1, Cooldown: time.Hour})
	d := notify.New(h.db, h.clk,
		[]notify.Channel{newFake(core.ChannelNtfy), newFake(core.ChannelTelegram)},
		notify.Options{Logger: quietLogger(), Breaker: b})

	preferred := []core.Channel{core.ChannelTelegram, core.ChannelNtfy, core.ChannelEmail}

	got := d.Available(preferred)
	if len(got) != 2 || got[0] != core.ChannelTelegram || got[1] != core.ChannelNtfy {
		t.Fatalf("available = %v, want telegram then ntfy (email is not registered)", got)
	}

	b.Fail(core.ChannelTelegram)
	got = d.Available(preferred)
	if len(got) != 1 || got[0] != core.ChannelNtfy {
		t.Errorf("available = %v, want only ntfy once telegram is open", got)
	}
}

// ─── Claiming ─────────────────────────────────────────────────────────────

// A worker that died mid-send leaves a row in sending. Reclaiming it is what
// stops the page disappearing with the process.
func TestStuckSendingRowsAreReclaimed(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.enqueue(core.ChannelNtfy, "sarthak", 0)

	// Simulate a crash: claimed, never resolved.
	if err := h.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE notifications SET state='sending' WHERE id=?`, id)
		return err
	}); err != nil {
		t.Fatalf("simulate crash: %v", err)
	}

	// Before the lease expires it stays put.
	claimed, err := h.db.ClaimDueNotifications(ctx, h.clk.Now(), time.Minute, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 0 {
		t.Errorf("claimed %d rows before the lease expired, want 0", len(claimed))
	}

	h.clk.Advance(2 * time.Minute)
	claimed, err = h.db.ClaimDueNotifications(ctx, h.clk.Now(), time.Minute, 10)
	if err != nil {
		t.Fatalf("claim after lease: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != id {
		t.Fatalf("claimed %v, want the stuck row %d", claimed, id)
	}
}

func TestClaimingIsExclusive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		h.enqueue(core.ChannelNtfy, fmt.Sprintf("user-%d", i), 0)
	}

	first, err := h.db.ClaimDueNotifications(ctx, h.clk.Now(), time.Minute, 5)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(first) != 5 {
		t.Fatalf("claimed %d, want 5", len(first))
	}

	// Everything is now in flight, so a second poll finds nothing.
	second, err := h.db.ClaimDueNotifications(ctx, h.clk.Now(), time.Minute, 5)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("a second claim took %d rows already in flight", len(second))
	}
}

func TestUnknownChannelDoesNotRetryForever(t *testing.T) {
	h := newHarness(t)
	// Dispatcher with no channels registered at all.
	d := notify.New(h.db, h.clk, nil, notify.Options{Logger: quietLogger()})

	id := h.enqueue(core.ChannelTelegram, "sarthak", 0)
	if _, err := d.DrainOnce(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n := h.notification(id); n.State != core.NotifDead {
		t.Errorf("state = %q, want dead; retrying an unregistered channel cannot help", n.State)
	}
}
