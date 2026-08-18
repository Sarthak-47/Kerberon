// Package group turns a stream of alerts into a small number of incidents.
//
// This is where alert fatigue is actually fought. A bad deploy can fire
// hundreds of alerts in a minute; the operator needs one page saying twelve
// services are down, not twelve pages. Three mechanisms do the work:
//
//   - Fingerprinting collapses repeats of the same alert, ignoring volatile
//     labels such as a pod name that changes on every reschedule.
//   - group_by collapses related alerts into one incident, enforced by a
//     partial unique index rather than by application-layer hope.
//   - group_wait delays the first page just long enough for a cascade to
//     arrive, so the notification describes the whole failure.
//
// Resolution is deliberately slow. An incident closes only once every alert in
// it has resolved and stayed resolved for resolve_grace. Without that window an
// alert oscillating every thirty seconds would produce a page-resolve-page
// storm.
package group

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/Sarthak-47/kerberon/internal/alert"
	"github.com/Sarthak-47/kerberon/internal/clock"
	"github.com/Sarthak-47/kerberon/internal/core"
	"github.com/Sarthak-47/kerberon/internal/route"
	"github.com/Sarthak-47/kerberon/internal/store"
	"github.com/Sarthak-47/kerberon/internal/timer"
)

// PageDue is called when an incident's group_wait window closes and it is time
// to page someone.
//
// It runs inside the timer's transaction and must therefore only write to the
// database — the escalation engine enqueues notifications to the outbox, and
// dispatch workers send them. See DECISIONS D1.
//
// Phase 3 supplies a recorder that only writes a timeline event; Phase 5
// replaces it with the escalation engine, with no change here.
type PageDue func(ctx context.Context, tx *sql.Tx, inc core.Incident) error

// Engine ingests alerts and maintains incidents.
type Engine struct {
	db     *store.DB
	clk    clock.Clock
	router *route.Router
	sched  *timer.Scheduler
	onPage PageDue
	log    *slog.Logger
}

// Options configures an Engine.
type Options struct {
	// OnPageDue is invoked when group_wait closes. Required.
	OnPageDue PageDue
	Logger    *slog.Logger
}

// New wires an Engine and registers its timer handlers.
func New(db *store.DB, clk clock.Clock, router *route.Router, sched *timer.Scheduler, opts Options) *Engine {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	onPage := opts.OnPageDue
	if onPage == nil {
		onPage = func(context.Context, *sql.Tx, core.Incident) error { return nil }
	}

	e := &Engine{db: db, clk: clk, router: router, sched: sched, onPage: onPage, log: log}
	sched.Register(core.TimerGroupWait, timer.HandlerFunc(e.handleGroupWait))
	sched.Register(core.TimerResolveTimeout, timer.HandlerFunc(e.handleResolveTimeout))
	return e
}

// Result summarizes what an ingest call did. It is what proves the cascade
// claim: hundreds of alerts, a handful of incidents.
type Result struct {
	// AlertsAccepted is how many alerts were stored.
	AlertsAccepted int
	// IncidentsCreated is how many new incidents opened.
	IncidentsCreated int
	// AlertsDeduplicated repeated a fingerprint already in their incident.
	AlertsDeduplicated int
	// Unrouted matched no route and therefore page nobody. Never silent.
	Unrouted int
}

// txChunk bounds how many alerts share one transaction.
//
// The write pool holds a single connection, so a transaction holds the only
// write path for its duration. Batching is what makes ingest fast, but an
// unbounded batch would stall every other writer — the ack handler, the
// scheduler — behind one enormous webhook.
const txChunk = 250

// Ingest stores alerts and attaches them to incidents.
//
// Alerts are committed in chunks rather than one transaction each. Measured on
// a 28-core machine, per-alert transactions gave 1,492 alerts/sec because the
// cost was 20,000 commits rather than the work itself; batching removes that
// floor. See DECISIONS.md D3, which deferred this until a benchmark showed it
// was needed.
//
// A chunk is all-or-nothing. That is the right trade for a webhook: the sender
// retries the whole payload, and a partially applied batch would leave an
// incident whose alert count disagrees with the alerts actually stored.
func (e *Engine) Ingest(ctx context.Context, alerts []core.Alert) (Result, error) {
	var res Result

	// Route first, outside any transaction: matching is pure computation and
	// an unrouted alert should not occupy the write connection at all.
	type routed struct {
		alert core.Alert
		route route.Route
	}
	pending := make([]routed, 0, len(alerts))

	for i := range alerts {
		a := alerts[i]
		rt, ok := e.router.Match(a.Labels)
		if !ok {
			// An unrouted alert pages nobody, which is the exact failure this
			// product exists to prevent. Log it loudly and count it so the
			// caller can surface it.
			res.Unrouted++
			e.log.Warn("alert matched no route and will never page",
				"labels", a.Labels, "source", a.Source)
			continue
		}
		a.Fingerprint = rt.Fingerprint(a.Labels)
		pending = append(pending, routed{alert: a, route: rt})
	}

	for start := 0; start < len(pending); start += txChunk {
		end := start + txChunk
		if end > len(pending) {
			end = len(pending)
		}
		chunk := pending[start:end]

		var chunkRes Result
		err := e.db.Tx(ctx, func(tx *sql.Tx) error {
			// Reset per attempt: a rolled-back transaction must not leave its
			// counts behind.
			chunkRes = Result{}
			for _, p := range chunk {
				created, deduped, err := e.applyAlert(ctx, tx, p.alert, p.route)
				if err != nil {
					return err
				}
				chunkRes.AlertsAccepted++
				if created {
					chunkRes.IncidentsCreated++
				}
				if deduped {
					chunkRes.AlertsDeduplicated++
				}
			}
			return nil
		})
		if err != nil {
			return res, err
		}
		res.AlertsAccepted += chunkRes.AlertsAccepted
		res.IncidentsCreated += chunkRes.IncidentsCreated
		res.AlertsDeduplicated += chunkRes.AlertsDeduplicated
	}

	if res.AlertsAccepted > 0 {
		// Timers were scheduled or cancelled; let the scheduler re-read.
		e.sched.Wake()
	}
	return res, nil
}

// applyAlert stores one alert inside a caller's transaction.
func (e *Engine) applyAlert(ctx context.Context, tx *sql.Tx, a core.Alert, rt route.Route) (created, deduped bool, err error) {
	groupKey := rt.GroupKey(a.Labels)
	now := e.clk.Now()

	inc, err := store.OpenIncidentByGroupKey(ctx, tx, groupKey)
	switch {
	case errors.Is(err, store.ErrNoOpenIncident):
		// A resolved alert with no open incident has nothing to close. Store
		// it for the record but do not open an incident, or a resolution would
		// page someone about a problem already over.
		if a.Status == core.AlertResolved {
			_, err := store.InsertAlert(ctx, tx, a)
			return false, false, err
		}
		return true, false, e.openIncident(ctx, tx, a, rt, groupKey, now)

	case err != nil:
		return false, false, err

	default:
		deduped, err := e.attachToIncident(ctx, tx, a, rt, inc, now)
		return false, deduped, err
	}
}

// openIncident creates an incident for a new group and starts its group_wait.
func (e *Engine) openIncident(ctx context.Context, tx *sql.Tx, a core.Alert, rt route.Route, groupKey string, now time.Time) error {
	inc := core.Incident{
		GroupKey:    groupKey,
		Team:        rt.Team,
		Policy:      rt.Policy,
		Severity:    alert.Severity(a.Labels),
		Title:       alert.Title(a),
		Status:      core.IncidentTriggered,
		CurrentStep: 0,
		AlertCount:  1,
		CreatedAt:   now,
		LastAlertAt: now,
	}

	id, err := store.InsertIncident(ctx, tx, inc)
	if err != nil {
		return err
	}
	inc.ID = id

	a.IncidentID = &id
	if _, err := store.InsertAlert(ctx, tx, a); err != nil {
		return err
	}

	detail, _ := json.Marshal(map[string]any{
		"route":       rt.Name,
		"group_key":   groupKey,
		"fingerprint": a.Fingerprint,
		"group_wait":  rt.GroupWait.String(),
	})
	if err := store.InsertEvent(ctx, tx, id, core.EventCreated, string(detail), now); err != nil {
		return err
	}

	// Hold the page for group_wait so a cascade can arrive and be described in
	// one notification rather than many.
	_, err = store.InsertTimer(ctx, tx, core.Timer{
		IncidentID: id,
		Kind:       core.TimerGroupWait,
		FireAt:     now.Add(rt.GroupWait),
		CreatedAt:  now,
	})
	return err
}

// attachToIncident adds an alert to an incident that is already open.
func (e *Engine) attachToIncident(ctx context.Context, tx *sql.Tx, a core.Alert, rt route.Route, inc core.Incident, now time.Time) (bool, error) {
	deduped, err := store.HasFingerprint(ctx, tx, inc.ID, a.Fingerprint)
	if err != nil {
		return false, err
	}

	a.IncidentID = &inc.ID
	if _, err := store.InsertAlert(ctx, tx, a); err != nil {
		return false, err
	}

	dedupIncrement := 0
	if deduped {
		dedupIncrement = 1
	}
	if err := store.TouchIncident(ctx, tx, inc.ID, now, 1, dedupIncrement); err != nil {
		return false, err
	}

	if a.Status == core.AlertFiring {
		// A re-fire cancels any pending resolution. This is the flapping
		// guard: an alert oscillating every thirty seconds must not produce a
		// page-resolve-page storm, and the existing incident simply continues.
		n, err := store.CancelIncidentTimers(ctx, tx, inc.ID, now, core.TimerResolveTimeout)
		if err != nil {
			return false, err
		}
		if n > 0 {
			detail, _ := json.Marshal(map[string]any{
				"reason":      "alert re-fired within resolve_grace",
				"fingerprint": a.Fingerprint,
			})
			if err := store.InsertEvent(ctx, tx, inc.ID, core.EventGrouped, string(detail), now); err != nil {
				return false, err
			}
		}
		return deduped, nil
	}

	// The alert resolved. If every alert in the group has now resolved, start
	// the grace window; the incident only closes if nothing re-fires.
	allResolved, err := store.AllAlertsResolved(ctx, tx, inc.ID)
	if err != nil {
		return false, err
	}
	if !allResolved {
		return deduped, nil
	}

	// Replace any existing grace timer so the window measures from the last
	// resolution rather than the first.
	if _, err := store.CancelIncidentTimers(ctx, tx, inc.ID, now, core.TimerResolveTimeout); err != nil {
		return false, err
	}
	_, err = store.InsertTimer(ctx, tx, core.Timer{
		IncidentID: inc.ID,
		Kind:       core.TimerResolveTimeout,
		FireAt:     now.Add(rt.ResolveGrace),
		CreatedAt:  now,
	})
	return deduped, err
}

// ─── Timer handlers ───────────────────────────────────────────────────────

// handleGroupWait closes the batching window and hands the incident to whoever
// pages. Everything happens in the timer's transaction (D1).
func (e *Engine) handleGroupWait(ctx context.Context, tx *sql.Tx, t core.Timer) error {
	inc, err := store.LoadIncident(ctx, tx, t.IncidentID)
	if err != nil {
		return err
	}
	// The incident may have been resolved or acknowledged during the window.
	if inc.Status != core.IncidentTriggered {
		return nil
	}

	detail, _ := json.Marshal(map[string]any{
		"alert_count": inc.AlertCount,
		"dedup_count": inc.DedupCount,
	})
	if err := store.InsertEvent(ctx, tx, inc.ID, core.EventGrouped, string(detail), e.clk.Now()); err != nil {
		return err
	}
	return e.onPage(ctx, tx, inc)
}

// handleResolveTimeout closes an incident once its grace window has passed.
//
// It re-checks resolution inside the transaction rather than trusting the
// state when the timer was scheduled: a re-fire that arrived moments ago must
// keep the incident open.
func (e *Engine) handleResolveTimeout(ctx context.Context, tx *sql.Tx, t core.Timer) error {
	allResolved, err := store.AllAlertsResolved(ctx, tx, t.IncidentID)
	if err != nil {
		return err
	}
	if !allResolved {
		// Something re-fired. The incident continues; the next resolution
		// schedules a fresh window.
		return nil
	}

	now := e.clk.Now()
	closed, err := store.ResolveIncident(ctx, tx, t.IncidentID, now, "auto")
	if err != nil {
		return err
	}
	if !closed {
		return nil
	}

	// A resolved incident has nothing left to escalate. This timer is excluded
	// from the cancellation: cancelling it would stop the scheduler marking it
	// complete, rolling back this whole transaction — resolution included — and
	// leaving it to fire again forever.
	if _, err := store.CancelIncidentTimersExcept(ctx, tx, t.IncidentID, t.ID, now); err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{"reason": "all alerts resolved and stayed resolved"})
	return store.InsertEvent(ctx, tx, t.IncidentID, core.EventResolved, string(detail), now)
}
