package escalate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Sarthak-47/kerberon/internal/ack"
	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/store"
	"github.com/Sarthak-47/kerberon/internal/timer"
)

// TargetResolver turns a step's targets into the users to page.
//
// It is called at the moment a step fires, not when the incident opened, so an
// incident spanning a handoff pages whoever is on call then rather than
// whoever has just gone off duty (spec section 8.1).
type TargetResolver interface {
	ResolveTargets(ctx context.Context, at time.Time, targets []Target) ([]string, error)
}

// ContactBook resolves a user's address on a channel.
type ContactBook interface {
	// Destination returns where to reach userID on ch. A user with no address
	// on a channel is skipped rather than failing the whole step, since the
	// other recipients of the same page still need to hear about it.
	Destination(userID string, ch core.Channel) (string, bool)
}

// Waker is the dispatcher, nudged after pages are enqueued.
type Waker interface{ Wake() }

// Engine advances incidents through their escalation policy.
type Engine struct {
	db       *store.DB
	clk      clock.Clock
	sched    *timer.Scheduler
	targets  TargetResolver
	contacts ContactBook
	signer   *ack.Signer
	baseURL  string
	waker    Waker
	log      *slog.Logger
}

// Options configures an Engine.
type Options struct {
	// ExternalURL is the address ack links are built from. It must be
	// reachable from the paged person's phone.
	ExternalURL string
	// Dispatcher is woken after pages are enqueued. Optional.
	Dispatcher Waker
	Logger     *slog.Logger
}

// New wires an Engine and registers its timer handlers.
func New(
	db *store.DB,
	clk clock.Clock,
	sched *timer.Scheduler,
	targets TargetResolver,
	contacts ContactBook,
	signer *ack.Signer,
	opts Options,
) *Engine {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	e := &Engine{
		db:       db,
		clk:      clk,
		sched:    sched,
		targets:  targets,
		contacts: contacts,
		signer:   signer,
		baseURL:  opts.ExternalURL,
		waker:    opts.Dispatcher,
		log:      log,
	}
	sched.Register(core.TimerEscalate, timer.HandlerFunc(e.handleEscalate))
	sched.Register(core.TimerRepeat, timer.HandlerFunc(e.handleAckTimeout))
	return e
}

// Begin starts escalation for an incident whose group_wait has closed.
//
// It matches group.PageDue, so the grouping engine hands off here with no
// knowledge of what paging involves.
func (e *Engine) Begin(ctx context.Context, tx *sql.Tx, inc core.Incident, policy Policy) error {
	snapshot, err := policy.Encode()
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE incidents SET policy_snapshot = ?, current_step = 0 WHERE id = ?`,
		snapshot, inc.ID); err != nil {
		return fmt.Errorf("store policy snapshot: %w", err)
	}
	inc.PolicySnapshot = snapshot
	inc.CurrentStep = 0

	return e.fireStep(ctx, tx, inc, policy, 0)
}

// handleEscalate advances an incident to its next step.
func (e *Engine) handleEscalate(ctx context.Context, tx *sql.Tx, t core.Timer) error {
	inc, err := store.LoadIncident(ctx, tx, t.IncidentID)
	if err != nil {
		return err
	}
	// An acknowledgement or a resolution between scheduling and firing means
	// there is nothing to escalate. Cancellation usually catches this; this is
	// the belt to that braces.
	if inc.Status != core.IncidentTriggered {
		return nil
	}

	policy, err := DecodePolicy(inc.PolicySnapshot)
	if err != nil {
		return err
	}

	var payload stepPayload
	if t.Payload != "" {
		if err := json.Unmarshal([]byte(t.Payload), &payload); err != nil {
			return fmt.Errorf("decode escalation timer payload: %w", err)
		}
	}
	return e.fireStep(ctx, tx, inc, policy, payload.Step)
}

// stepPayload carries which step a timer represents, so a timer that fires
// late cannot advance the wrong rung.
type stepPayload struct {
	Step int `json:"step"`
}

// fireStep pages the targets of one step and schedules what comes next.
func (e *Engine) fireStep(ctx context.Context, tx *sql.Tx, inc core.Incident, policy Policy, index int) error {
	now := e.clk.Now()

	step, pass, ok := policy.StepAt(index)
	if !ok {
		return e.expire(ctx, tx, inc, policy, now)
	}

	users, err := e.targets.ResolveTargets(ctx, now, step.Targets)
	if err != nil {
		return fmt.Errorf("resolve targets for step %d: %w", index, err)
	}

	if len(users) == 0 {
		// Nobody is on call for this step. That is a coverage gap arriving at
		// the worst possible moment, so it is recorded loudly and escalation
		// continues to the next rung rather than stopping here.
		e.log.Error("escalation step resolved to nobody; advancing immediately",
			"incident_id", inc.ID, "step", index, "targets", targetNames(step.Targets))
		detail, _ := json.Marshal(map[string]any{
			"step":    index,
			"targets": targetNames(step.Targets),
			"reason":  "no user is on call for these targets",
		})
		if err := store.InsertEvent(ctx, tx, inc.ID, core.EventEscalated, string(detail), now); err != nil {
			return err
		}
	}

	enqueued := 0
	for _, user := range users {
		for _, ch := range step.Channels {
			dest, ok := e.contacts.Destination(user, ch)
			if !ok {
				// kerberon validate refuses a config that can reach this, but
				// a live reload could still introduce it.
				e.log.Warn("user has no address on a channel this step pages",
					"incident_id", inc.ID, "user", user, "channel", ch)
				continue
			}

			n := core.Notification{
				IncidentID:     inc.ID,
				IdempotencyKey: store.IdempotencyKey(inc.ID, index, user, ch, pass),
				StepIndex:      index,
				TargetUser:     user,
				Channel:        ch,
				Destination:    dest,
				Body:           e.body(inc, user, index),
				Title:          inc.Title,
				Severity:       inc.Severity,
				AckURL:         e.ackLink(inc, user, index),
				State:          core.NotifPending,
				CreatedAt:      now,
			}
			switch _, err := store.EnqueueNotification(ctx, tx, n); {
			case errors.Is(err, store.ErrDuplicateNotification):
				// Already queued for this exact step and pass. This is the
				// mechanism working, not a problem.
				continue
			case err != nil:
				return err
			}
			enqueued++
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE incidents SET current_step = ? WHERE id = ?`, index, inc.ID); err != nil {
		return fmt.Errorf("advance incident step: %w", err)
	}

	detail, _ := json.Marshal(map[string]any{
		"step":     index,
		"pass":     pass,
		"users":    users,
		"channels": step.Channels,
		"pages":    enqueued,
	})
	if err := store.InsertEvent(ctx, tx, inc.ID, core.EventNotified, string(detail), now); err != nil {
		return err
	}

	// Schedule the next rung. Its delay is measured from now, which is why a
	// step's delay is the gap after the previous step rather than an offset
	// from incident creation.
	next := index + 1
	if policy.Exhausted(next) {
		return e.scheduleExpiry(ctx, tx, inc, policy, next, now)
	}
	nextStep, _, _ := policy.StepAt(next)
	return e.scheduleStep(ctx, tx, inc.ID, next, now.Add(nextStep.Delay.Std()), now)
}

// scheduleStep queues the timer for the next rung.
func (e *Engine) scheduleStep(ctx context.Context, tx *sql.Tx, incidentID int64, step int, fireAt, now time.Time) error {
	payload, err := json.Marshal(stepPayload{Step: step})
	if err != nil {
		return err
	}
	_, err = store.InsertTimer(ctx, tx, core.Timer{
		IncidentID: incidentID,
		Kind:       core.TimerEscalate,
		FireAt:     fireAt,
		Payload:    string(payload),
		CreatedAt:  now,
	})
	return err
}

// scheduleExpiry queues the timer that will expire an unanswered incident,
// giving the final step its full delay before declaring nobody answered.
func (e *Engine) scheduleExpiry(ctx context.Context, tx *sql.Tx, inc core.Incident, policy Policy, index int, now time.Time) error {
	last := policy.Steps[len(policy.Steps)-1]
	return e.scheduleStep(ctx, tx, inc.ID, index, now.Add(last.Delay.Std()), now)
}

// expire marks an incident nobody answered.
//
// This is a loud, visible state rather than a silent drop: "nobody answered"
// is critical information (spec section 6.4).
func (e *Engine) expire(ctx context.Context, tx *sql.Tx, inc core.Incident, policy Policy, now time.Time) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE incidents SET status = 'expired' WHERE id = ? AND status = 'triggered'`, inc.ID)
	if err != nil {
		return fmt.Errorf("expire incident %d: %w", inc.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // acknowledged or resolved in the meantime
	}

	e.log.Error("INCIDENT EXPIRED: escalation exhausted every step and nobody acknowledged",
		"incident_id", inc.ID, "team", inc.Team, "policy", policy.Name,
		"title", inc.Title, "passes", policy.TotalPasses())

	detail, _ := json.Marshal(map[string]any{
		"policy": policy.Name,
		"passes": policy.TotalPasses(),
		"reason": "escalation exhausted without an acknowledgement",
	})
	return store.InsertEvent(ctx, tx, inc.ID, core.EventExpired, string(detail), now)
}

// ─── Acknowledgement ──────────────────────────────────────────────────────

// ErrNotAcknowledgeable reports that an incident could not be acknowledged
// because it is already acknowledged, resolved or expired. It is a no-op
// rather than a failure: an ack arriving just after a resolution is ordinary.
var ErrNotAcknowledgeable = errors.New("incident is not awaiting acknowledgement")

// Acknowledge records that a human took the incident.
//
// It cancels pending escalation but does not resolve anything: someone
// answering the page is not the same as the problem being over.
func (e *Engine) Acknowledge(ctx context.Context, incidentID int64, userID string, via core.AckVia) error {
	now := e.clk.Now()

	err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		took, err := store.AcknowledgeIncident(ctx, tx, incidentID, now, userID)
		if err != nil {
			return err
		}
		if !took {
			return ErrNotAcknowledgeable
		}

		if _, err := store.CancelIncidentTimers(ctx, tx, incidentID, now,
			core.TimerEscalate); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO acks (incident_id, user_id, via, created_at) VALUES (?, ?, ?, ?)`,
			incidentID, userID, string(via), now.Unix()); err != nil {
			return fmt.Errorf("record ack: %w", err)
		}

		inc, err := store.LoadIncident(ctx, tx, incidentID)
		if err != nil {
			return err
		}
		// Re-escalate if an acknowledged incident is not resolved in time.
		// This catches "acknowledged and fell back asleep", which is a real
		// thing (spec section 8.1).
		if policy, err := DecodePolicy(inc.PolicySnapshot); err == nil {
			if d := policy.AckTimeout.Std(); d > 0 {
				if _, err := store.InsertTimer(ctx, tx, core.Timer{
					IncidentID: incidentID,
					Kind:       core.TimerRepeat,
					FireAt:     now.Add(d),
					CreatedAt:  now,
				}); err != nil {
					return err
				}
			}
		}

		detail, _ := json.Marshal(map[string]any{"user": userID, "via": string(via)})
		return store.InsertEvent(ctx, tx, incidentID, core.EventAcked, string(detail), now)
	})
	if err != nil {
		return err
	}

	e.sched.Wake()
	e.log.Info("incident acknowledged", "incident_id", incidentID, "user", userID, "via", via)
	return nil
}

// handleAckTimeout resumes escalation for an incident that was acknowledged
// but never resolved.
func (e *Engine) handleAckTimeout(ctx context.Context, tx *sql.Tx, t core.Timer) error {
	inc, err := store.LoadIncident(ctx, tx, t.IncidentID)
	if err != nil {
		return err
	}
	if inc.Status != core.IncidentAcknowledged {
		return nil // resolved, or escalation already resumed
	}

	policy, err := DecodePolicy(inc.PolicySnapshot)
	if err != nil {
		return err
	}
	now := e.clk.Now()

	if _, err := tx.ExecContext(ctx,
		`UPDATE incidents SET status = 'triggered' WHERE id = ? AND status = 'acknowledged'`,
		inc.ID); err != nil {
		return fmt.Errorf("resume escalation for incident %d: %w", inc.ID, err)
	}

	e.log.Warn("acknowledged incident was not resolved in time; resuming escalation",
		"incident_id", inc.ID, "acknowledged_by", inc.AcknowledgedBy,
		"ack_timeout", policy.AckTimeout.Std())

	detail, _ := json.Marshal(map[string]any{
		"reason":          "ack_timeout elapsed without resolution",
		"acknowledged_by": inc.AcknowledgedBy,
	})
	if err := store.InsertEvent(ctx, tx, inc.ID, core.EventEscalated, string(detail), now); err != nil {
		return err
	}

	inc.Status = core.IncidentTriggered
	return e.fireStep(ctx, tx, inc, policy, inc.CurrentStep+1)
}

// ─── Helpers ──────────────────────────────────────────────────────────────

// ackLink is the one-tap acknowledgement URL for this recipient at this step,
// or empty when acknowledgement is not configured.
func (e *Engine) ackLink(inc core.Incident, userID string, step int) string {
	if e.signer == nil || e.baseURL == "" {
		return ""
	}
	return e.signer.Link(e.baseURL, inc.ID, userID, step)
}

// body composes what the human reads.
//
// The link is repeated in the text as well as carried in AckURL, because a
// channel that cannot render a button — email, a plain webhook — still has to
// give the reader something to tap.
func (e *Engine) body(inc core.Incident, userID string, step int) string {
	link := e.ackLink(inc, userID, step)

	msg := fmt.Sprintf("[%s] %s", inc.Severity, inc.Title)
	if inc.AlertCount > 1 {
		msg += fmt.Sprintf("\n%d alerts in this incident", inc.AlertCount)
	}
	msg += fmt.Sprintf("\nteam: %s", inc.Team)
	if link != "" {
		msg += "\nacknowledge: " + link
	}
	return msg
}

func targetNames(targets []Target) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.String())
	}
	return out
}

// Wake nudges the dispatcher, if one is attached. Call after committing a
// transaction that enqueued pages.
func (e *Engine) Wake() {
	if e.waker != nil {
		e.waker.Wake()
	}
}
