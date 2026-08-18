package notify

import (
	"sync"
	"time"

	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/core"
)

// Breaker trips a channel out of service after repeated failures.
//
// Without one, a provider that is down absorbs every retry in turn and the page
// queues behind it. The point is not to protect the provider — it is that if
// Telegram is broken the page must still go out over ntfy rather than waiting
// in line for something that is not coming back (spec section 8.3).
type Breaker struct {
	clk       clock.Clock
	threshold int
	cooldown  time.Duration

	mu    sync.Mutex
	state map[core.Channel]*breakerState
}

type breakerState struct {
	consecutiveFailures int
	openUntil           time.Time
}

// BreakerOptions tunes a Breaker. The zero value gives sensible defaults.
type BreakerOptions struct {
	// Threshold is how many consecutive failures open the breaker.
	Threshold int
	// Cooldown is how long it stays open before one attempt is let through.
	Cooldown time.Duration
}

func (o BreakerOptions) withDefaults() BreakerOptions {
	if o.Threshold <= 0 {
		o.Threshold = 5
	}
	if o.Cooldown <= 0 {
		o.Cooldown = time.Minute
	}
	return o
}

// NewBreaker creates a Breaker.
func NewBreaker(clk clock.Clock, opts BreakerOptions) *Breaker {
	opts = opts.withDefaults()
	return &Breaker{
		clk:       clk,
		threshold: opts.Threshold,
		cooldown:  opts.Cooldown,
		state:     make(map[core.Channel]*breakerState),
	}
}

// Allow reports whether a send may be attempted on this channel.
//
// Once the cooldown elapses a single attempt is admitted: success closes the
// breaker, failure re-opens it. That probe is what lets a recovered provider
// come back into service without anyone intervening.
func (b *Breaker) Allow(ch core.Channel) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	st, ok := b.state[ch]
	if !ok {
		return true
	}
	if st.openUntil.IsZero() {
		return true
	}
	return !b.clk.Now().Before(st.openUntil)
}

// Succeed records a delivery, closing the breaker.
func (b *Breaker) Succeed(ch core.Channel) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.state, ch)
}

// Fail records a failure and opens the breaker once the threshold is reached.
// It reports whether the breaker is now open.
func (b *Breaker) Fail(ch core.Channel) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	st, ok := b.state[ch]
	if !ok {
		st = &breakerState{}
		b.state[ch] = st
	}
	st.consecutiveFailures++

	if st.consecutiveFailures >= b.threshold {
		st.openUntil = b.clk.Now().Add(b.cooldown)
		return true
	}
	return false
}

// Open reports whether the breaker for a channel is currently open. Used by
// the UI and by tests; the dispatcher asks Allow.
func (b *Breaker) Open(ch core.Channel) bool { return !b.Allow(ch) }

// Failures reports the consecutive failure count for a channel.
func (b *Breaker) Failures(ch core.Channel) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if st, ok := b.state[ch]; ok {
		return st.consecutiveFailures
	}
	return 0
}
