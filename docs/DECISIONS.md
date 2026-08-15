# Architecture Decisions

Decisions taken on top of `KERBERON_PROJECT_SPEC.md` v1.0. Each records what was
decided and why. Where a decision amends the spec, that is stated explicitly.

Status key: **Accepted** — decided, build to it. **Open** — see spec §15.

---

## D1. Timer effects are database-only, applied in a single transaction

**Accepted.** Amends spec §7.3.

The spec's loop claims a timer in one step and executes its effect "in the same
transaction" in the next. Those cannot both hold. If claim and execute are separate
transactions, a crash between them leaves `claimed_at` set and `completed_at` null;
the recovery pass re-selects the timer, the claim `UPDATE ... WHERE claimed_at IS NULL`
matches zero rows, and the timer is skipped permanently. That is a silently missed
escalation — the exact failure the design exists to prevent.

Resolution: a timer's effect **must be purely a database state change**. Advancing
`current_step`, inserting rows into `notifications`, and scheduling the next timer are
all DB writes. Nothing with an external side effect may run inside a timer handler.
The whole tick becomes one transaction:

```
BEGIN
  SELECT the due timer (completed_at IS NULL AND cancelled_at IS NULL)
  apply effect: advance incident, enqueue notifications, schedule next timer
  UPDATE timers SET completed_at = ?
COMMIT
```

Exactly-once then follows from SQLite's atomicity rather than from application logic.
A crash mid-transaction rolls back cleanly and the recovery pass re-runs the tick.

`claimed_at` is retained in the schema but **unused in v1**. It is reserved for the
lease-based claim that leader election will require if HA is ever added (spec §15.7),
so that path needs no migration.

**Enforcement:** timer handlers take a `*sql.Tx` and return `error`. No HTTP client,
channel client, or `notify` package is reachable from the escalation engine.

---

## D2. All database writes go through a single writer

**Accepted.** Extends spec §4.3.

WAL permits concurrent readers alongside **one** writer. Kerberon has many writers
(ingest, dispatch workers, ack handler, heartbeat sweeper, UI), so `busy_timeout=5000`
would convert contention into multi-second stalls on the ingest path.

- Two pools over the same file: `writeDB` with `MaxOpenConns(1)`, `readDB` with N.
  Writes serialize in Go's connection pool instead of contending for the SQLite lock,
  which also removes a class of flaky test.
- `synchronous=NORMAL`. In WAL mode this remains crash-safe against process death —
  what the chaos suite tests — and only risks the last few commits on OS or power
  failure. `FULL` fsyncs every commit and caps throughput at disk fsync rate. This is
  a deliberate trade, documented for users.
- Pragmas: `journal_mode=WAL`, `busy_timeout=5000`, `foreign_keys=ON`,
  `synchronous=NORMAL`.

---

## D3. Ingest is built behind a batching seam; the batcher itself is deferred

**Accepted.** Amends spec §11 target.

One transaction per HTTP request will not reach 5,000 alerts/sec — that rate needs
alerts arriving within a short window coalesced into one transaction.

However, the 5,000/sec figure is not grounded in a real deployment: Alertmanager
already batches alerts into grouped webhooks, so a 50-engineer team sees orders of
magnitude less. Building a batching pipeline for a number nobody hits is premature.

Resolution: the write path is defined as an interface that submits work to the single
writer, so a batching implementation can be dropped in without touching callers. The
batcher is **not** built until Week 7 benchmarks show it is needed. The README
publishes the number actually measured, not the aspirational one.

---

## D4. Incidents snapshot policy structure; targets resolve live

**Accepted.** Resolves a gap in spec §4.2 / §8.1.

Config is hot-reloadable, but the spec does not say what happens to an in-flight
incident when a reload removes or alters the escalation policy it is walking through.

- **Policy structure** (steps, delays, channels, repeat, ack_timeout) is snapshotted
  onto the incident as JSON at creation. An incident escalates the way it started,
  even across a reload that deletes the policy.
- **Targets** resolve live at the moment each step fires, as spec §8.1 requires — so
  an incident spanning a handoff pages whoever is genuinely on call now, not whoever
  was on call when it opened.

The rejected alternative — refusing a reload that would orphan an open incident — is
simpler but blocks a config fix at precisely the worst moment.

---

## D5. `Clock` owns every interaction with time

**Accepted.** Strengthens spec §4.4.

A `Clock` exposing only `Now()` leaves the scheduler calling real `time.Sleep`, which
makes every escalation test either slow or nondeterministic and quietly defeats the
rule. The interface covers waiting as well as reading:

```go
type Clock interface {
    Now() time.Time
    After(d time.Duration) <-chan time.Time
    NewTicker(d time.Duration) Ticker
    Sleep(ctx context.Context, d time.Duration) error
}
```

`FakeClock.Advance(d)` releases every waiter whose deadline has passed and blocks
until those goroutines have made progress, so tests cannot race the clock.

**CI grep bans, outside `internal/clock`:** `time.Now`, `time.Sleep`, `time.After`,
`time.NewTimer`, `time.NewTicker`, `time.Tick`.

---

## D6. The schedule resolver's primitive is intervals, not point lookups

**Accepted.** Extends spec §7.1.

`resolve(schedule, at) → user` serves `/api/v1/oncall` and step-fire target
resolution, but three consumers need more: the 30-day calendar, coverage-gap detection
in `kerberon validate`, and the 400-day no-gap/no-overlap property test. None can be
built correctly from point queries — hourly sampling would miss a short gap at a DST
boundary, which is the precise bug the property test exists to catch. A sampling
implementation would pass while being wrong.

```go
Intervals(from, to time.Time) []Interval  // primitive: {start, end, userID}
At(t time.Time) (userID string, ok bool)  // derived
```

`Intervals` merges each layer's enumerated transition instants into a sorted boundary
set, evaluates layer priority per sub-interval, and coalesces adjacent equal segments.
Coverage gaps then appear as literal holes in the output. DST correctness lives in one
place — each layer's boundary enumeration — rather than smeared across the resolver.

---

## D7. Notification delivery is at-least-once at the channel boundary

**Accepted.** Resolves a gap in spec §8.2/§8.3.

A worker marks a notification `sending`, makes the HTTP call, and crashes before
marking `sent`. On restart the outcome is unknowable. The idempotency key does not
help — it prevents duplicate *enqueue*, not duplicate *send*.

Resolution: **retry.** A duplicate push is an annoyance; a missed page is a failure.
Rows stuck in `sending` beyond a lease (default 2m) are reclaimed. Kerberon documents
that it is at-least-once at the channel boundary and a human may occasionally receive
a duplicate notification following a crash.

---

## D8. Package structure inverts the timer/escalation dependency

**Accepted.** Refines spec §4.4.

`escalate` needs `timer` to schedule work, and `timer` needs to invoke effects that
live in `escalate` — a cycle. Inverted: `internal/timer` is generic, knowing only
kinds, payloads, and a handler registry; `escalate` registers its handlers at startup.

`internal/core` is added, holding the shared domain types (`Alert`, `Incident`,
`Notification`, `Interval`) so no package imports `store` merely for a struct.

---

## D9. Build order swaps timers ahead of ingest

**Accepted.** Amends spec §12 (weeks 2 and 3).

`group_wait` is itself a durable timer and is the first thing the grouping engine
needs. Building grouping first means faking the wait in memory and rewriting it a week
later. Timers depend only on the store and the clock, so they can follow the skeleton
immediately.

Revised order — skeleton → **timers** → **ingest/grouping** → schedules →
escalation/notify → heartbeats/UI → hardening → launch. Same eight weeks, same weekly
deliverables, one fewer rewrite.

---

## D10. The chaos suite is Linux-only and driven by config-shortened delays

**Accepted.** Constrains spec §11.

Primary development is on Windows, which has no `SIGKILL`; process-termination
semantics differ enough that the test would not measure the same property. The chaos
suite runs on Linux only — WSL or Docker locally, Linux CI for the nightly 1,000
iterations.

A fake clock cannot span a real process restart, so the harness uses the real clock
with escalation delays shortened to tens of milliseconds. **Delays must therefore be
config-driven from Week 1**, not hardcoded, or the flagship test is infeasible to run
at the required iteration count.

---

## Open, carried from spec §15

Not decided here; raise with the author before the affected work begins.

| # | Question | Blocks |
|---|---|---|
| 1 | Name and domain availability; `kerb3` fallback | README, install script, Go module path |
| 4 | Absolute TTL on ack links, on top of incident-lifetime expiry | Week 5 |
| 5 | `ack_timeout` re-escalation default-on or opt-in | Week 5 |
| 6 | Retention policy (proposal: 90 days, nightly vacuum) | Week 7 |
| 7 | HA path and leader election sketch | v2, but see D1 |
| 8 | Slack app in v1.1 | post-v1 |

Spec §15.2 (Go vs Rust) and §15.3 (HTMX vs SPA) are treated as settled in favour of
the spec's assumptions: Go, because static cross-compilation *is* the product; HTMX,
revisited only if the calendar view proves painful server-rendered.
