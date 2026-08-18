// Package notify delivers pages.
//
// The escalation engine never sends anything itself. It writes rows to the
// notifications table in the same transaction that advances the incident, and
// the workers here pick them up. That split is what makes a crash between
// "state advanced" and "page sent" impossible to lose (spec section 8.2).
//
// Delivery is at-least-once. A worker that dies after the outbound call but
// before recording success leaves an ambiguous row, and the ambiguity is
// resolved by retrying: a duplicate page is an annoyance, a missed page is a
// failure (DECISIONS D7).
package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Sarthak-47/kerberon/internal/core"
)

// Message is one page, ready to send.
type Message struct {
	IncidentID int64
	// Destination is the address resolved at send time — a topic URL, a chat
	// id, an email address.
	Destination string
	Title       string
	Body        string
	Severity    core.Severity
	// AckLink is the one-tap acknowledgement URL. Channels that can render a
	// button should use it as the target.
	AckLink string
}

// Channel delivers a Message.
//
// Adding a channel is one file implementing this interface plus one line
// registering it; the dispatcher never changes (spec section 8.5).
type Channel interface {
	// Name is the identifier used in config and in the notifications table.
	Name() core.Channel
	// Send delivers the message. It must respect ctx cancellation.
	Send(ctx context.Context, m Message) error
	// Capabilities describes what the channel can do, so the dispatcher can
	// choose between equivalent options without special-casing any of them.
	Capabilities() Capabilities
}

// Capabilities describes a channel's behaviour.
type Capabilities struct {
	// SupportsButtons is true where the message can carry an inline
	// acknowledge control rather than only a link.
	SupportsButtons bool
	// IsLoud is true where delivery is likely to wake somebody — a push with
	// high priority, an SMS, a phone call — as opposed to an email that waits
	// until morning.
	IsLoud bool
}

// ErrRetryable marks a failure worth trying again: a timeout, a 5xx, a
// connection reset.
//
// The distinction matters because retrying an unretryable failure wastes the
// window in which someone could still be paged by another means. A malformed
// destination will not fix itself, and the incident is better served by failing
// over to another channel immediately.
var ErrRetryable = errors.New("retryable delivery failure")

// Retryable wraps err so the dispatcher will schedule another attempt.
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrRetryable, err.Error())
}

// IsRetryable reports whether a delivery failure should be retried.
func IsRetryable(err error) bool { return errors.Is(err, ErrRetryable) }

// ─── Retry schedule ───────────────────────────────────────────────────────

// backoffSteps is the retry schedule from spec section 8.3.
var backoffSteps = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	45 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
}

// Backoff returns how long to wait before attempt number n (1-based), with
// full jitter.
//
// Full jitter rather than a fixed schedule so that a provider coming back up
// does not immediately receive every queued retry at the same instant. jitter
// must return a value in [0, 1).
func Backoff(attempt int, jitter func() float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	idx := attempt - 1
	if idx >= len(backoffSteps) {
		idx = len(backoffSteps) - 1
	}
	base := backoffSteps[idx]
	if jitter == nil {
		return base
	}
	// Full jitter: anywhere in [0, base]. The upper bound is preserved so the
	// schedule never stretches beyond what was configured.
	return time.Duration(float64(base) * jitter())
}

// MaxAttempts is the default before a notification is declared dead.
const MaxAttempts = 5
