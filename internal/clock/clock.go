// Package clock is the only package in Kerberon permitted to touch the wall
// clock directly. Everything else takes a Clock, which is what makes the
// escalation engine, the timer scheduler and the schedule resolver testable
// without sleeping.
//
// The interface deliberately covers waiting as well as reading. A Clock that
// exposed only Now would leave callers reaching for time.Sleep, and a test
// suite that sleeps is a test suite that flakes in CI. See docs/DECISIONS.md D5.
package clock

import (
	"context"
	"time"
)

// Timer is the subset of *time.Timer that Kerberon uses. It is an interface so
// FakeClock can supply its own implementation.
type Timer interface {
	// C returns the channel on which the tick is delivered.
	C() <-chan time.Time
	// Stop prevents the timer from firing. It reports whether the call stopped
	// the timer before it fired.
	Stop() bool
	// Reset changes the timer to expire after d. It reports whether the timer
	// was active.
	Reset(d time.Duration) bool
}

// Ticker is the subset of *time.Ticker that Kerberon uses.
type Ticker interface {
	C() <-chan time.Time
	Stop()
	Reset(d time.Duration)
}

// Clock is the source of time for every subsystem.
type Clock interface {
	// Now returns the current time.
	Now() time.Time

	// Since is shorthand for Now().Sub(t).
	Since(t time.Time) time.Duration

	// After returns a channel that receives once d has elapsed.
	After(d time.Duration) <-chan time.Time

	// NewTimer creates a Timer that fires once after d.
	NewTimer(d time.Duration) Timer

	// NewTicker creates a Ticker that fires every d. d must be > 0.
	NewTicker(d time.Duration) Ticker

	// Sleep blocks for d, or until ctx is done. It returns ctx.Err() if the
	// context ended first, and nil if the full duration elapsed.
	Sleep(ctx context.Context, d time.Duration) error
}

// Real returns a Clock backed by the system clock.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) Since(t time.Time) time.Duration        { return time.Since(t) }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (realClock) NewTimer(d time.Duration) Timer {
	return &realTimer{t: time.NewTimer(d)}
}

func (realClock) NewTicker(d time.Duration) Ticker {
	return &realTicker{t: time.NewTicker(d)}
}

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type realTimer struct{ t *time.Timer }

func (r *realTimer) C() <-chan time.Time        { return r.t.C }
func (r *realTimer) Stop() bool                 { return r.t.Stop() }
func (r *realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }

type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time   { return r.t.C }
func (r *realTicker) Stop()                 { r.t.Stop() }
func (r *realTicker) Reset(d time.Duration) { r.t.Reset(d) }
