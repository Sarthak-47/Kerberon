package clock

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// FakeClock is a deterministic Clock for tests. Time only moves when Advance or
// Set is called.
//
// The usual hazard with a fake clock is racing it: the test advances time before
// the code under test has registered its waiter, the tick is delivered to nobody,
// and the test hangs or passes for the wrong reason. BlockUntil closes that race
// by waiting until the expected number of waiters exist:
//
//	c := clock.NewFake(t0)
//	go scheduler.Run(ctx, c)   // will call c.After(...) internally
//	c.BlockUntil(1)            // wait until it is actually waiting
//	c.Advance(5 * time.Minute) // now the tick cannot be missed
//
// Ticks are delivered in deadline order, and Now reports each waiter's own
// deadline at the moment it fires — not the post-Advance time — so code that
// timestamps its work during a multi-tick Advance records the times it would
// have seen on a real clock.
type FakeClock struct {
	mu      sync.Mutex
	cond    *sync.Cond
	now     time.Time
	waiters []*fakeWaiter
}

// NewFake returns a FakeClock set to now.
func NewFake(now time.Time) *FakeClock {
	f := &FakeClock{now: now}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// NewFakeAt returns a FakeClock set to the given RFC3339 instant. It panics on a
// malformed value, which is what you want in a test helper.
func NewFakeAt(rfc3339 string) *FakeClock {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		panic(fmt.Sprintf("clock: NewFakeAt(%q): %v", rfc3339, err))
	}
	return NewFake(t)
}

var _ Clock = (*FakeClock)(nil)

func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *FakeClock) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

func (f *FakeClock) After(d time.Duration) <-chan time.Time { return f.NewTimer(d).C() }

func (f *FakeClock) NewTimer(d time.Duration) Timer {
	return f.addWaiter(d, 0)
}

func (f *FakeClock) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: NewTicker requires a positive period")
	}
	return &tickerAdapter{w: f.addWaiter(d, d)}
}

func (f *FakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := f.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Advance moves time forward by d, delivering every tick that falls in the
// interval, in deadline order. It panics on a negative duration: time in
// Kerberon never runs backwards, and a negative advance is always a test bug.
func (f *FakeClock) Advance(d time.Duration) {
	if d < 0 {
		panic("clock: Advance called with a negative duration")
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	target := f.now.Add(d)
	for {
		w := f.earliestDueLocked(target)
		if w == nil {
			break
		}
		// Report the waiter's own deadline while it fires, not the final time.
		f.now = w.deadline
		w.fire(f.now)
		if w.period > 0 {
			w.deadline = w.deadline.Add(w.period)
		} else {
			f.removeLocked(w)
		}
	}
	f.now = target
	f.cond.Broadcast()
}

// Set moves the clock to t, delivering any ticks in between. It panics if t is
// before the current time.
func (f *FakeClock) Set(t time.Time) {
	f.mu.Lock()
	now := f.now
	f.mu.Unlock()
	if t.Before(now) {
		panic(fmt.Sprintf("clock: Set(%s) is before the current time %s", t, now))
	}
	f.Advance(t.Sub(now))
}

// BlockUntil blocks until at least n waiters are registered on this clock.
// Use it before Advance to eliminate the race between a goroutine starting to
// wait and the test moving time.
func (f *FakeClock) BlockUntil(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.waiters) < n {
		f.cond.Wait()
	}
}

// Waiters reports how many timers and tickers are currently waiting. Useful for
// asserting that cancellation actually released them.
func (f *FakeClock) Waiters() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.waiters)
}

func (f *FakeClock) addWaiter(d, period time.Duration) *fakeWaiter {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fakeWaiter{
		clock: f,
		// Buffered by one, matching time.Timer and time.Ticker, so delivery
		// never blocks Advance.
		ch:       make(chan time.Time, 1),
		deadline: f.now.Add(d),
		period:   period,
	}
	f.waiters = append(f.waiters, w)
	f.cond.Broadcast()

	// A non-positive one-shot duration is already due, matching time.NewTimer.
	if period == 0 && d <= 0 {
		w.fire(f.now)
		f.removeLocked(w)
	}
	return w
}

// earliestDueLocked returns the waiter with the smallest deadline at or before
// target, or nil. The caller must hold f.mu.
func (f *FakeClock) earliestDueLocked(target time.Time) *fakeWaiter {
	var best *fakeWaiter
	for _, w := range f.waiters {
		if w.deadline.After(target) {
			continue
		}
		if best == nil || w.deadline.Before(best.deadline) {
			best = w
		}
	}
	return best
}

// removeLocked drops w from the waiter list. The caller must hold f.mu.
func (f *FakeClock) removeLocked(w *fakeWaiter) bool {
	for i, cand := range f.waiters {
		if cand == w {
			f.waiters = append(f.waiters[:i], f.waiters[i+1:]...)
			f.cond.Broadcast()
			return true
		}
	}
	return false
}

type fakeWaiter struct {
	clock    *FakeClock
	ch       chan time.Time
	deadline time.Time
	period   time.Duration
}

// fire delivers a tick without blocking. A full buffer means the previous tick
// has not been consumed, and the new one is dropped — the same coalescing
// behaviour as time.Ticker.
func (w *fakeWaiter) fire(at time.Time) {
	select {
	case w.ch <- at:
	default:
	}
}

func (w *fakeWaiter) C() <-chan time.Time { return w.ch }

func (w *fakeWaiter) Stop() bool {
	w.clock.mu.Lock()
	defer w.clock.mu.Unlock()
	return w.clock.removeLocked(w)
}

func (w *fakeWaiter) Reset(d time.Duration) bool {
	w.clock.mu.Lock()
	defer w.clock.mu.Unlock()
	wasActive := false
	for _, cand := range w.clock.waiters {
		if cand == w {
			wasActive = true
			break
		}
	}
	w.deadline = w.clock.now.Add(d)
	if !wasActive {
		w.clock.waiters = append(w.clock.waiters, w)
		w.clock.cond.Broadcast()
	}
	return wasActive
}

// Ticker requires Stop with no return value; Timer requires bool. fakeWaiter
// satisfies Timer directly, and tickerAdapter narrows it for Ticker.
var (
	_ Timer  = (*fakeWaiter)(nil)
	_ Ticker = (*tickerAdapter)(nil)
)

type tickerAdapter struct{ w *fakeWaiter }

func (t *tickerAdapter) C() <-chan time.Time { return t.w.C() }
func (t *tickerAdapter) Stop()               { t.w.Stop() }

// Reset changes the period as well as the next deadline, matching time.Ticker.
func (t *tickerAdapter) Reset(d time.Duration) {
	if d <= 0 {
		panic("clock: Ticker.Reset requires a positive period")
	}
	t.w.clock.mu.Lock()
	t.w.period = d
	t.w.clock.mu.Unlock()
	t.w.Reset(d)
}
