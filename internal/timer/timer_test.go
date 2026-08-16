package timer_test

import (
	"context"
	"database/sql"
	"errors"
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
	"github.com/Sarthak-47/kerberon/internal/store"
	"github.com/Sarthak-47/kerberon/internal/timer"
)

const t0RFC = "2026-08-15T09:00:00Z"

// ─── Harness ──────────────────────────────────────────────────────────────

type harness struct {
	t          *testing.T
	db         *store.DB
	clk        *clock.FakeClock
	sched      *timer.Scheduler
	incidentID int64

	cancel context.CancelFunc
	done   chan struct{}
}

// newHarness opens a migrated database with one incident to hang timers off.
// Scratch files live under the project's .tmp, never the system temp directory
// (CLAUDE.md R1).
func newHarness(t *testing.T) *harness {
	t.Helper()

	root := filepath.Join("..", "..", ".tmp")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create .tmp: %v", err)
	}
	dir, err := os.MkdirTemp(root, "timer-test-")
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
	h.sched = timer.New(db, h.clk, timer.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

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

// start runs the scheduler until the test ends.
func (h *harness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		defer close(h.done)
		if err := h.sched.Run(ctx); err != nil {
			h.t.Errorf("scheduler returned %v", err)
		}
	}()
	h.t.Cleanup(h.stop)
}

func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case <-h.done:
	//kerberon:allow-clock -- real deadline so a hung test fails instead of blocking CI
	case <-time.After(5 * time.Second):
		h.t.Error("scheduler did not stop within 5s")
	}
	h.cancel = nil
}

// waitIdle blocks until the scheduler is asleep on the fake clock, which means
// it has finished all work it can currently do. Advancing before this point
// would race the scheduler's registration of its waiter.
func (h *harness) waitIdle() {
	h.t.Helper()
	h.clk.BlockUntil(1)
}

// schedule inserts a timer due after d.
func (h *harness) schedule(kind core.TimerKind, d time.Duration) int64 {
	h.t.Helper()
	id, err := h.sched.ScheduleNow(context.Background(), core.Timer{
		IncidentID: h.incidentID,
		Kind:       kind,
		FireAt:     h.clk.Now().Add(d),
		CreatedAt:  h.clk.Now(),
	})
	if err != nil {
		h.t.Fatalf("schedule: %v", err)
	}
	return id
}

func (h *harness) timer(id int64) core.Timer {
	h.t.Helper()
	tm, err := h.db.Timer(context.Background(), id)
	if err != nil {
		h.t.Fatalf("read timer %d: %v", id, err)
	}
	return tm
}

// fired is a handler that counts executions and signals each one.
type fired struct {
	count  atomic.Int64
	signal chan int64 // receives the timer id
}

func newFired() *fired {
	return &fired{signal: make(chan int64, 64)}
}

func (f *fired) handler() timer.HandlerFunc {
	return func(ctx context.Context, tx *sql.Tx, t core.Timer) error {
		f.count.Add(1)
		f.signal <- t.ID
		return nil
	}
}

// await waits for one execution.
func (f *fired) await(t *testing.T, what string) int64 {
	t.Helper()
	select {
	case id := <-f.signal:
		return id
	//kerberon:allow-clock -- real deadline so a hung test fails instead of blocking CI
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: handler never ran", what)
		return 0
	}
}

func (f *fired) expectNoMore(t *testing.T, what string) {
	t.Helper()
	select {
	case id := <-f.signal:
		t.Fatalf("%s: handler ran again for timer %d", what, id)
	default:
	}
}

// ─── Basic firing ─────────────────────────────────────────────────────────

func TestTimerFiresWhenDue(t *testing.T) {
	h := newHarness(t)
	f := newFired()
	h.sched.Register(core.TimerEscalate, f.handler())

	id := h.schedule(core.TimerEscalate, 5*time.Minute)
	h.start()
	h.waitIdle()

	h.clk.Advance(5 * time.Minute)
	if got := f.await(t, "escalation"); got != id {
		t.Fatalf("fired timer %d, want %d", got, id)
	}

	h.waitIdle()
	tm := h.timer(id)
	if tm.CompletedAt == nil {
		t.Error("timer was not marked completed")
	}
	if tm.Pending() {
		t.Error("timer is still pending after firing")
	}
}

func TestTimerDoesNotFireEarly(t *testing.T) {
	h := newHarness(t)
	f := newFired()
	h.sched.Register(core.TimerEscalate, f.handler())

	h.schedule(core.TimerEscalate, 5*time.Minute)
	h.start()
	h.waitIdle()

	h.clk.Advance(4*time.Minute + 59*time.Second)
	h.waitIdle()
	f.expectNoMore(t, "before the deadline")

	h.clk.Advance(time.Second)
	f.await(t, "at the deadline")
}

// The central guarantee: never zero times, never twice.
func TestTimerFiresExactlyOnce(t *testing.T) {
	h := newHarness(t)
	f := newFired()
	h.sched.Register(core.TimerEscalate, f.handler())

	h.schedule(core.TimerEscalate, time.Minute)
	h.start()
	h.waitIdle()

	h.clk.Advance(time.Minute)
	f.await(t, "first fire")

	// Push time far past the deadline repeatedly. A timer that is not properly
	// completed would fire again on each pass.
	for i := 0; i < 5; i++ {
		h.waitIdle()
		h.clk.Advance(time.Hour)
	}
	h.waitIdle()

	if got := f.count.Load(); got != 1 {
		t.Fatalf("handler ran %d times, want exactly 1", got)
	}
}

// ─── Ordering and recovery ────────────────────────────────────────────────

// Crash recovery replays a backlog of overdue timers. They must run oldest
// first, or escalation steps land out of order.
func TestOverdueTimersRunInFireAtOrder(t *testing.T) {
	h := newHarness(t)

	var (
		mu    sync.Mutex
		order []int64
	)
	done := make(chan struct{})
	h.sched.Register(core.TimerEscalate, timer.HandlerFunc(
		func(ctx context.Context, tx *sql.Tx, tm core.Timer) error {
			mu.Lock()
			order = append(order, tm.ID)
			n := len(order)
			mu.Unlock()
			if n == 4 {
				close(done)
			}
			return nil
		}))

	// Insert out of chronological order; the scheduler must sort by fire_at.
	third := h.schedule(core.TimerEscalate, 30*time.Minute)
	first := h.schedule(core.TimerEscalate, 10*time.Minute)
	fourth := h.schedule(core.TimerEscalate, 40*time.Minute)
	second := h.schedule(core.TimerEscalate, 20*time.Minute)

	// Simulate a process that was down: jump past every deadline before the
	// scheduler starts.
	h.clk.Advance(time.Hour)
	h.start()

	select {
	case <-done:
	//kerberon:allow-clock -- real deadline so a hung test fails instead of blocking CI
	case <-time.After(5 * time.Second):
		t.Fatal("not all overdue timers fired")
	}

	mu.Lock()
	defer mu.Unlock()
	want := []int64{first, second, third, fourth}
	if len(order) != len(want) {
		t.Fatalf("fired %d timers, want %d", len(order), len(want))
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("position %d: fired timer %d, want %d (order was %v, want %v)",
				i, order[i], want[i], order, want)
		}
	}
}

// A restart must not re-run work that already committed.
func TestCompletedTimersAreNotRerunAfterRestart(t *testing.T) {
	h := newHarness(t)
	f := newFired()
	h.sched.Register(core.TimerEscalate, f.handler())

	h.schedule(core.TimerEscalate, time.Minute)
	h.start()
	h.waitIdle()
	h.clk.Advance(time.Minute)
	f.await(t, "first run")
	h.waitIdle()
	h.stop()

	// A fresh scheduler over the same database, as a restart would be.
	f2 := newFired()
	h.sched = timer.New(h.db, h.clk, timer.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	h.sched.Register(core.TimerEscalate, f2.handler())
	h.start()
	h.waitIdle()
	h.clk.Advance(time.Hour)
	h.waitIdle()

	if got := f2.count.Load(); got != 0 {
		t.Fatalf("a completed timer ran again after restart (%d times)", got)
	}
}

// ─── Cancellation ─────────────────────────────────────────────────────────

func TestCancelledTimerNeverFires(t *testing.T) {
	h := newHarness(t)
	f := newFired()
	h.sched.Register(core.TimerEscalate, f.handler())

	id := h.schedule(core.TimerEscalate, 5*time.Minute)
	h.start()
	h.waitIdle()

	if err := h.sched.Cancel(context.Background(), id); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	h.waitIdle()
	h.clk.Advance(time.Hour)
	h.waitIdle()

	if got := f.count.Load(); got != 0 {
		t.Fatalf("a cancelled timer fired %d times", got)
	}
	if h.timer(id).CancelledAt == nil {
		t.Error("cancelled_at was not set")
	}
}

// An acknowledgement cancels every pending escalation timer for an incident.
func TestCancelForIncidentStopsEscalation(t *testing.T) {
	h := newHarness(t)
	f := newFired()
	h.sched.Register(core.TimerEscalate, f.handler())
	h.sched.Register(core.TimerResolveTimeout, f.handler())

	h.schedule(core.TimerEscalate, 5*time.Minute)
	h.schedule(core.TimerEscalate, 10*time.Minute)
	resolve := h.schedule(core.TimerResolveTimeout, 15*time.Minute)

	h.start()
	h.waitIdle()

	// Cancel only escalation, as an ack does: the resolve timeout survives.
	n, err := h.sched.CancelForIncident(context.Background(), h.incidentID, core.TimerEscalate)
	if err != nil {
		t.Fatalf("cancel for incident: %v", err)
	}
	if n != 2 {
		t.Errorf("cancelled %d timers, want 2", n)
	}

	h.waitIdle()
	h.clk.Advance(time.Hour)

	if got := f.await(t, "resolve timeout"); got != resolve {
		t.Errorf("fired timer %d, want the resolve timeout %d", got, resolve)
	}
	h.waitIdle()
	if got := f.count.Load(); got != 1 {
		t.Fatalf("handler ran %d times, want 1 (only the uncancelled timer)", got)
	}
}

// Cancelling something already finished is normal, not an error: an ack landing
// as a step fires is an ordinary race.
func TestCancelIsIdempotent(t *testing.T) {
	h := newHarness(t)
	id := h.schedule(core.TimerEscalate, time.Minute)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := h.sched.Cancel(ctx, id); err != nil {
			t.Fatalf("cancel %d: %v", i, err)
		}
	}
	if h.timer(id).Pending() {
		t.Error("timer still pending after cancel")
	}
}

// ─── Atomicity ────────────────────────────────────────────────────────────

// The effect and the completion share one transaction. A handler that fails
// must leave no trace and the timer must stay pending — which is exactly what
// a crash mid-effect looks like.
func TestFailedHandlerRollsBackItsEffectAndLeavesTimerPending(t *testing.T) {
	h := newHarness(t)
	boom := errors.New("handler exploded")

	var attempts atomic.Int64
	h.sched.Register(core.TimerEscalate, timer.HandlerFunc(
		func(ctx context.Context, tx *sql.Tx, tm core.Timer) error {
			attempts.Add(1)
			// A real effect: write to the audit trail, then fail.
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO events (incident_id, kind, created_at) VALUES (?, 'escalated', 1)`,
				tm.IncidentID); err != nil {
				return err
			}
			return boom
		}))

	id := h.schedule(core.TimerEscalate, time.Minute)
	h.start()
	h.waitIdle()
	h.clk.Advance(time.Minute)

	// Wait for at least one attempt.
	//kerberon:allow-clock -- real wall-clock guard against an infinite retry loop
	deadline := time.Now().Add(5 * time.Second)
	//kerberon:allow-clock
	for attempts.Load() == 0 && time.Now().Before(deadline) {
		h.waitIdle()
		h.clk.Advance(time.Second)
	}
	if attempts.Load() == 0 {
		t.Fatal("handler never ran")
	}
	h.waitIdle()

	var events int
	if err := h.db.Read().QueryRow(`SELECT COUNT(*) FROM events`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 0 {
		t.Errorf("%d event rows survived a failed handler; the effect was not rolled back", events)
	}

	tm := h.timer(id)
	if !tm.Pending() {
		t.Error("timer is no longer pending after a failed handler; the escalation was lost")
	}
	if tm.CompletedAt != nil {
		t.Error("a failed timer was marked completed")
	}
}

// A handler scheduling the next step must commit atomically with the current
// step's completion — that is how an escalation chain advances.
func TestHandlerCanScheduleTheNextTimerInTheSameTransaction(t *testing.T) {
	h := newHarness(t)

	var steps atomic.Int64
	done := make(chan struct{})
	h.sched.Register(core.TimerEscalate, timer.HandlerFunc(
		func(ctx context.Context, tx *sql.Tx, tm core.Timer) error {
			n := steps.Add(1)
			if n >= 3 {
				close(done)
				return nil
			}
			_, err := store.InsertTimer(ctx, tx, core.Timer{
				IncidentID: tm.IncidentID,
				Kind:       core.TimerEscalate,
				FireAt:     h.clk.Now().Add(5 * time.Minute),
				CreatedAt:  h.clk.Now(),
			})
			return err
		}))

	h.schedule(core.TimerEscalate, 5*time.Minute)
	h.start()

	for i := 0; i < 3; i++ {
		h.waitIdle()
		h.clk.Advance(5 * time.Minute)
	}

	select {
	case <-done:
	//kerberon:allow-clock -- real deadline so a hung test fails instead of blocking CI
	case <-time.After(5 * time.Second):
		t.Fatalf("escalation chain stalled after %d steps", steps.Load())
	}
}

// ─── Unknown kinds ────────────────────────────────────────────────────────

// A timer whose kind this build does not know is most plausibly a downgrade.
// Leaving the row pending preserves the escalation for an upgraded binary;
// completing it would silently drop a page.
func TestUnknownKindIsDeferredNotCompleted(t *testing.T) {
	h := newHarness(t)
	// Register nothing.

	id := h.schedule(core.TimerRepeat, time.Minute)
	h.start()
	h.waitIdle()
	h.clk.Advance(time.Minute)
	h.waitIdle()
	h.clk.Advance(time.Minute)
	h.waitIdle()

	tm := h.timer(id)
	if tm.CompletedAt != nil {
		t.Error("a timer with no handler was marked completed; the escalation was lost")
	}
	if !tm.Pending() {
		t.Error("a timer with no handler is no longer pending")
	}
}

// One failing timer must not prevent others from running.
func TestOneFailingTimerDoesNotBlockOthers(t *testing.T) {
	h := newHarness(t)

	good := newFired()
	h.sched.Register(core.TimerGroupWait, good.handler())
	h.sched.Register(core.TimerEscalate, timer.HandlerFunc(
		func(ctx context.Context, tx *sql.Tx, tm core.Timer) error {
			return errors.New("always fails")
		}))

	// The failing timer is due first.
	h.schedule(core.TimerEscalate, time.Minute)
	h.schedule(core.TimerGroupWait, 2*time.Minute)

	h.start()
	h.waitIdle()
	h.clk.Advance(3 * time.Minute)

	// The good timer must still fire despite the earlier one failing.
	good.await(t, "group_wait behind a failing escalate")
}

// ─── Store-level behaviour ────────────────────────────────────────────────

func TestInsertTimerRejectsInvalidInput(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := h.clk.Now()

	cases := []struct {
		name string
		tm   core.Timer
	}{
		{"unknown kind", core.Timer{IncidentID: h.incidentID, Kind: "bogus", FireAt: now, CreatedAt: now}},
		{"no incident", core.Timer{Kind: core.TimerEscalate, FireAt: now, CreatedAt: now}},
		{"no created_at", core.Timer{IncidentID: h.incidentID, Kind: core.TimerEscalate, FireAt: now}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := h.db.Tx(ctx, func(tx *sql.Tx) error {
				_, err := store.InsertTimer(ctx, tx, c.tm)
				return err
			})
			if err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestCompleteTimerTwiceReportsNotPending(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	id := h.schedule(core.TimerEscalate, time.Minute)

	err := h.db.Tx(ctx, func(tx *sql.Tx) error {
		return store.CompleteTimer(ctx, tx, id, h.clk.Now())
	})
	if err != nil {
		t.Fatalf("first complete: %v", err)
	}

	err = h.db.Tx(ctx, func(tx *sql.Tx) error {
		return store.CompleteTimer(ctx, tx, id, h.clk.Now())
	})
	if !errors.Is(err, store.ErrTimerNotPending) {
		t.Fatalf("second complete returned %v, want ErrTimerNotPending", err)
	}
}

func TestLoadPendingTimerRejectsFinishedTimers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	cancelled := h.schedule(core.TimerEscalate, time.Minute)
	if err := h.sched.Cancel(ctx, cancelled); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	err := h.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.LoadPendingTimer(ctx, tx, cancelled)
		return err
	})
	if !errors.Is(err, store.ErrTimerNotPending) {
		t.Fatalf("load cancelled timer returned %v, want ErrTimerNotPending", err)
	}

	err = h.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.LoadPendingTimer(ctx, tx, 999999)
		return err
	})
	if !errors.Is(err, store.ErrTimerNotPending) {
		t.Fatalf("load missing timer returned %v, want ErrTimerNotPending", err)
	}
}

func TestPendingTimersExcludesFinished(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	live := h.schedule(core.TimerEscalate, time.Minute)
	cancelled := h.schedule(core.TimerEscalate, 2*time.Minute)
	if err := h.sched.Cancel(ctx, cancelled); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	pending, err := h.db.PendingTimers(ctx)
	if err != nil {
		t.Fatalf("pending timers: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != live {
		t.Fatalf("pending = %v, want only timer %d", pending, live)
	}
}

// Timers are cascade-deleted with their incident, so retention pruning cannot
// strand them.
func TestTimersAreDeletedWithTheirIncident(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.schedule(core.TimerEscalate, time.Minute)

	err := h.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM incidents WHERE id = ?`, h.incidentID)
		return err
	})
	if err != nil {
		t.Fatalf("delete incident: %v", err)
	}

	pending, err := h.db.PendingTimers(ctx)
	if err != nil {
		t.Fatalf("pending timers: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("%d timers survived their incident", len(pending))
	}
}
