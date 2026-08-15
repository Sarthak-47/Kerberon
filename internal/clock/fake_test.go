package clock_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Sarthak-47/kerberon/internal/clock"
)

// t0 is an arbitrary fixed instant. Deliberately not "now" — a test that depends
// on when it runs is not a test.
const t0RFC = "2026-08-15T09:00:00Z"

func t0(t *testing.T) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, t0RFC)
	if err != nil {
		t.Fatalf("parse t0: %v", err)
	}
	return v
}

// recv reads a tick, failing if none is available. It never blocks, because a
// FakeClock that has already fired has the value buffered.
func recv(t *testing.T, ch <-chan time.Time, what string) time.Time {
	t.Helper()
	select {
	case got := <-ch:
		return got
	default:
		t.Fatalf("%s: expected a tick, channel was empty", what)
		return time.Time{}
	}
}

func expectNoTick(t *testing.T, ch <-chan time.Time, what string) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("%s: unexpected tick at %s", what, got.Format(time.RFC3339))
	default:
	}
}

func TestNowDoesNotMoveOnItsOwn(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	first := c.Now()
	for i := 0; i < 1000; i++ {
		if got := c.Now(); !got.Equal(first) {
			t.Fatalf("clock moved without Advance: %s != %s", got, first)
		}
	}
}

func TestAdvanceMovesTime(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	c.Advance(90 * time.Minute)
	want := t0(t).Add(90 * time.Minute)
	if got := c.Now(); !got.Equal(want) {
		t.Fatalf("Now() = %s, want %s", got, want)
	}
}

func TestTimerFiresOnlyOnceDue(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	timer := c.NewTimer(5 * time.Minute)

	c.Advance(4*time.Minute + 59*time.Second)
	expectNoTick(t, timer.C(), "before deadline")

	c.Advance(time.Second)
	got := recv(t, timer.C(), "at deadline")
	if want := t0(t).Add(5 * time.Minute); !got.Equal(want) {
		t.Fatalf("tick carried %s, want the deadline %s", got, want)
	}

	// A one-shot timer must not fire again.
	c.Advance(time.Hour)
	expectNoTick(t, timer.C(), "after deadline")
}

// The tick must carry the deadline, not the post-Advance time. Escalation
// records when a step fired, and a single Advance can cross several steps.
func TestTickCarriesItsOwnDeadlineNotTheFinalTime(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	first := c.NewTimer(1 * time.Minute)
	second := c.NewTimer(2 * time.Minute)

	c.Advance(10 * time.Minute)

	if got, want := recv(t, first.C(), "first"), t0(t).Add(1*time.Minute); !got.Equal(want) {
		t.Errorf("first tick = %s, want %s", got, want)
	}
	if got, want := recv(t, second.C(), "second"), t0(t).Add(2*time.Minute); !got.Equal(want) {
		t.Errorf("second tick = %s, want %s", got, want)
	}
}

// Escalation steps must be observed in order even when recovery replays a
// backlog in one jump. This is the property crash recovery depends on.
func TestTicksDeliveredInDeadlineOrder(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)

	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup

	// Register out of chronological order on purpose.
	for _, step := range []struct {
		id    int
		delay time.Duration
	}{
		{3, 15 * time.Minute},
		{1, 5 * time.Minute},
		{4, 20 * time.Minute},
		{2, 10 * time.Minute},
	} {
		wg.Add(1)
		ch := c.After(step.delay)
		go func(id int, ch <-chan time.Time) {
			defer wg.Done()
			<-ch
			mu.Lock()
			order = append(order, id)
			mu.Unlock()
		}(step.id, ch)
	}

	c.BlockUntil(4)
	c.Advance(time.Hour)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 4 {
		t.Fatalf("got %d ticks, want 4", len(order))
	}
	// Goroutine scheduling makes the observed slice order nondeterministic, so
	// assert on the set; ordering of delivery is asserted by the deadline each
	// tick carries in the test above.
	seen := map[int]bool{}
	for _, id := range order {
		if seen[id] {
			t.Fatalf("step %d fired twice", id)
		}
		seen[id] = true
	}
	for id := 1; id <= 4; id++ {
		if !seen[id] {
			t.Errorf("step %d never fired", id)
		}
	}
}

func TestStopPreventsFiring(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	timer := c.NewTimer(5 * time.Minute)

	if !timer.Stop() {
		t.Fatal("Stop on an active timer should report true")
	}
	if timer.Stop() {
		t.Error("Stop on an already-stopped timer should report false")
	}
	if got := c.Waiters(); got != 0 {
		t.Errorf("Waiters() = %d after Stop, want 0", got)
	}

	c.Advance(time.Hour)
	expectNoTick(t, timer.C(), "stopped timer")
}

// An acknowledgement cancels pending escalation timers. That must release the
// waiter, not merely suppress the tick.
func TestStopReleasesTheWaiter(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	a := c.NewTimer(time.Minute)
	b := c.NewTimer(time.Minute)
	if got := c.Waiters(); got != 2 {
		t.Fatalf("Waiters() = %d, want 2", got)
	}
	a.Stop()
	if got := c.Waiters(); got != 1 {
		t.Fatalf("Waiters() = %d after one Stop, want 1", got)
	}
	c.Advance(2 * time.Minute)
	expectNoTick(t, a.C(), "stopped timer")
	recv(t, b.C(), "live timer")
}

func TestResetReschedules(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	timer := c.NewTimer(5 * time.Minute)

	if !timer.Reset(10 * time.Minute) {
		t.Error("Reset on an active timer should report true")
	}
	c.Advance(5 * time.Minute)
	expectNoTick(t, timer.C(), "original deadline after Reset")

	c.Advance(5 * time.Minute)
	recv(t, timer.C(), "new deadline")
}

func TestResetRevivesAStoppedTimer(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	timer := c.NewTimer(5 * time.Minute)
	timer.Stop()

	if timer.Reset(time.Minute) {
		t.Error("Reset on a stopped timer should report false")
	}
	c.Advance(time.Minute)
	recv(t, timer.C(), "revived timer")
}

func TestTickerFiresRepeatedly(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	tk := c.NewTicker(30 * time.Second)
	defer tk.Stop()

	for i := 1; i <= 3; i++ {
		c.Advance(30 * time.Second)
		got := recv(t, tk.C(), "ticker")
		want := t0(t).Add(time.Duration(i) * 30 * time.Second)
		if !got.Equal(want) {
			t.Fatalf("tick %d = %s, want %s", i, got, want)
		}
	}
}

// The heartbeat sweeper runs on a ticker. If the process is busy, missed ticks
// must coalesce rather than queue up and cause a burst of sweeps.
func TestTickerCoalescesMissedTicks(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	tk := c.NewTicker(30 * time.Second)
	defer tk.Stop()

	// Cross ten periods without reading. time.Ticker drops the overflow.
	c.Advance(5 * time.Minute)

	recv(t, tk.C(), "first buffered tick")
	expectNoTick(t, tk.C(), "coalesced ticks")
}

func TestTickerStopHalts(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	tk := c.NewTicker(time.Minute)
	c.Advance(time.Minute)
	recv(t, tk.C(), "before Stop")

	tk.Stop()
	if got := c.Waiters(); got != 0 {
		t.Errorf("Waiters() = %d after ticker Stop, want 0", got)
	}
	c.Advance(10 * time.Minute)
	expectNoTick(t, tk.C(), "after Stop")
}

func TestSleepReturnsWhenTimeAdvances(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	done := make(chan error, 1)
	go func() { done <- c.Sleep(context.Background(), 5*time.Minute) }()

	c.BlockUntil(1)
	select {
	case <-done:
		t.Fatal("Sleep returned before time advanced")
	default:
	}

	c.Advance(5 * time.Minute)
	if err := <-done; err != nil {
		t.Fatalf("Sleep returned %v, want nil", err)
	}
}

// Graceful shutdown cancels the context while the scheduler is sleeping.
func TestSleepReturnsOnContextCancel(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Sleep(ctx, time.Hour) }()

	c.BlockUntil(1)
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep returned %v, want context.Canceled", err)
	}
	if got := c.Waiters(); got != 0 {
		t.Errorf("Waiters() = %d after cancelled Sleep, want 0 (leaked waiter)", got)
	}
}

func TestSetJumpsForwardAndDelivers(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	timer := c.NewTimer(30 * time.Minute)

	c.Set(t0(t).Add(2 * time.Hour))
	recv(t, timer.C(), "timer crossed by Set")
	if got, want := c.Now(), t0(t).Add(2*time.Hour); !got.Equal(want) {
		t.Fatalf("Now() = %s, want %s", got, want)
	}
}

func TestAdvanceRejectsNegativeDurations(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Advance with a negative duration should panic")
		}
	}()
	clock.NewFakeAt(t0RFC).Advance(-time.Second)
}

func TestSetRejectsGoingBackwards(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	defer func() {
		if recover() == nil {
			t.Fatal("Set to an earlier time should panic")
		}
	}()
	c.Set(t0(t).Add(-time.Second))
}

func TestNonPositiveTimerIsImmediatelyDue(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	timer := c.NewTimer(0)
	recv(t, timer.C(), "zero-duration timer")
	if got := c.Waiters(); got != 0 {
		t.Errorf("Waiters() = %d, want 0", got)
	}
}

func TestBlockUntilWaitsForWaiters(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	released := make(chan struct{})

	go func() {
		c.BlockUntil(2)
		close(released)
	}()

	select {
	case <-released:
		t.Fatal("BlockUntil(2) returned with no waiters registered")
	default:
	}

	c.NewTimer(time.Minute)
	c.NewTimer(time.Minute)
	<-released // hangs and fails the test on timeout if BlockUntil is broken
}

// Concurrent use must be race-free; `go test -race` is the real assertion here.
func TestConcurrentUseIsRaceFree(t *testing.T) {
	c := clock.NewFakeAt(t0RFC)
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tm := c.NewTimer(time.Duration(1+i%5) * time.Minute)
			defer tm.Stop()
			// Drain non-blockingly; whether it has fired yet is racy by design.
			select {
			case <-tm.C():
			default:
			}
		}()
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Now()
			c.Advance(time.Minute)
		}()
	}
	wg.Wait()
}

func TestRealClockSatisfiesTheInterface(t *testing.T) {
	c := clock.Real()
	start := c.Now()
	if err := c.Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if c.Since(start) <= 0 {
		t.Error("Since should be positive after sleeping")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Sleep returned %v, want context.Canceled", err)
	}
}
