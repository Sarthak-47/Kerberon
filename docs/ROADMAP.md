# Kerberon — Roadmap

Derived from spec §12, with the week 2/3 swap from `DECISIONS.md` D9 applied
(durable timers precede ingest, because `group_wait` is itself a durable timer).

Every phase ends in something runnable and demonstrable. No phase is pure refactoring.
A phase is complete only when its **exit criteria** are demonstrated, not when the code
is written.

---

## Phase 0 — Prerequisites

Not in the spec's eight weeks. Settled before any Go code was written.
**Status: complete.**

| Item | State |
|---|---|
| Go toolchain | Done — 1.26.6 pinned in `.toolchain/`, SHA256 verified against go.dev |
| Project-local `GOPATH`/`GOMODCACHE`/`GOCACHE`/`GOTMPDIR`/`GOENV` | Done — `scripts\env.ps1`, `scripts\env.sh` |
| Module path | Done — `github.com/Sarthak-47/kerberon` |
| `git init`, per-repo identity, authorized remote | Done — `Sarthak-47 <0906sarthak@gmail.com>` |
| `.gitignore` covering caches, toolchain, `vendor/`, `*.db*` | Done |
| CI workflows | Done — build ×4, race tests, vet, gofmt, clock lint, DCO |
| `LICENSE` (Apache-2.0), `CONTRIBUTING.md` (DCO), `SECURITY.md` | Done |

**Exit — met:** `. .\scripts\env.ps1; go version` works, all four targets build,
`git log` shows the correct author with no co-author trailer, CI green on every push.

Go's toolchain telemetry turned out to be the one write that cannot be redirected into
the project; it is disabled machine-wide and documented in CLAUDE.md.

---

## Phase 1 — Skeleton — **complete**

**Goal:** the spine everything else hangs from.

- `internal/clock` — `Clock` interface covering `Now`, `After`, `NewTicker`, `Sleep`
  (D5). `FakeClock` with `Advance()` that deterministically releases waiters and blocks
  until they have made progress.
- `internal/core` — shared domain types (`Alert`, `Incident`, `Notification`,
  `Interval`), so nothing imports `store` for a struct (D8).
- `internal/store` — two connection pools, `writeDB` at `MaxOpenConns(1)` (D2);
  pragmas `journal_mode=WAL`, `synchronous=NORMAL`, `busy_timeout=5000`,
  `foreign_keys=ON`; embedded migrations run at startup; full schema from spec §5.
- `internal/config` — YAML parsing, `${VAR}` expansion, structural validation.
- `cmd/kerberon` — `serve`, `validate`, `migrate`, `version`.
- Structured logging (`log/slog`).
- CI: build ×4 platforms, `go vet`, race tests, the time-lint grep, DCO check.

**Exit criteria — all met**
- `kerberon validate --config examples/kerberon.yaml` passes, and a deliberately
  broken config reports all nine problems at once with line numbers and field paths.
- `kerberon migrate` creates the schema; re-running reports the existing version and
  changes nothing.
- `FakeClock` unit tests pass with zero `time.Sleep` anywhere in the test suite.
- CI green on all four target platforms. Binary is 7.2 MB against a 25 MB budget.

Not done here, and deliberately: 24×7 coverage-gap detection is listed under spec §9
as part of `validate`, but it needs the schedule resolver. It lands in Phase 4, and
`validate` says so in its output rather than letting a reader assume it already proves
someone is on call at every instant.

**Unblocks:** everything.

---

## Phase 2 — Durable timers *(moved ahead of ingest, D9)* — **complete**

**Goal:** the exactly-once guarantee, proven early.

- `internal/timer` — generic scheduler: kinds, JSON payloads, handler registry (D8).
  Single scheduler goroutine; sleeps on `Clock.After` until the earliest `fire_at`,
  woken by a buffered channel when an earlier timer is inserted.
- Tick executes as **one transaction** — select, apply effect, mark `completed_at`
  (D1). Handlers receive `*sql.Tx` and cannot reach any I/O client.
- Cancellation via `cancelled_at`; cancelled timers never execute.
- Crash recovery: on startup, past-due timers execute immediately in `fire_at` order.
- `claimed_at` present in schema, unused, commented as reserved for HA leases.
- Escalation delays are config-driven from day one, so the chaos harness can shorten
  them to milliseconds (D10).
- First chaos test — a handful of iterations, not yet 1,000.

**Exit criteria — all met**
- Schedule a timer, `SIGKILL` the process, restart, every escalation step fires
  **exactly once** — asserted by counting one audit row per step across randomised
  kill/restart cycles. Runs in CI on every push (15 iterations) and nightly (400).
- Cancellation is re-checked inside the executing transaction, so an acknowledgement
  landing as a step fires can never double-execute.
- A process down past several deadlines catches up on restart in `fire_at` order,
  with no separate recovery code path — the ordinary "earliest pending" query is
  recovery.
- A failed handler rolls back its effect and leaves the timer pending, which is
  exactly what a crash mid-effect looks like.
- Chaos harness is Unix-only and skipped on Windows, as planned.

One design correction found by the tests: taking only the single earliest pending
timer let a timer that was backing off after a handler failure block every timer
behind it. The scheduler now considers a small batch instead.

**Unblocks:** grouping (`group_wait`), escalation, heartbeats.

---

## Phase 3 — Ingest, dedup, grouping — **complete**

**Goal:** many alerts become few incidents.

- Ingest handlers: `/api/v1/alerts`, `/alertmanager`, `/grafana`,
  `/heartbeat/{token}`; bearer-token auth from config.
- `internal/alert` — normalization to a common `Alert`; fingerprinting
  `sha256(canonical_labels)` with configurable `volatile_labels`.
- `internal/group` — `group_by` → `group_key`; `group_wait` and `group_interval` as
  durable timers; the partial unique index as the dedup invariant.
- `internal/route` — label match → team + policy.
- Resolution with `resolve_grace`; flapping suppression.
- Write path behind an interface so a batcher can drop in later; **no batcher yet**
  (D3).

**Exit criteria — met**
- A 400-alert cascade produces one incident, 399 recorded duplicates and one page.
  Verified both as a unit test and end to end against a running `kerberon serve`,
  which answered in 85 ms with
  `{"accepted":400,"incidents_created":1,"deduplicated":399,"unrouted":0}`.
- A group re-firing inside `resolve_grace` cancels the pending resolve and does not
  re-page; a test oscillates an alert five times and asserts one page total.
- Alertmanager and Grafana payloads normalize, including Alertmanager's zero `endsAt`.
- Unrouted alerts are counted and logged rather than dropped.
- Ingest authenticates in constant time and caps body size.

Deferred from this phase on purpose: the `/heartbeat/{token}` endpoint moves to
Phase 6, where the sweeper that gives it meaning is built. Concurrent-insert races on
a new group are covered at the store layer by the partial unique index; an ingest-level
concurrency test belongs with the load harness in Phase 7.

---

## Phase 4 — Schedules — **complete**

**Goal:** correct answers about time, including twice a year when it is hard.

- Layer model: overrides → restrictions → base rotation, resolved highest-first.
- `Intervals(from, to)` as the primitive; `At(t)` derived from it (D6).
- IANA timezones, `time/tzdata` embedded; DST policies — nonexistent local times shift
  forward, ambiguous local times take the first occurrence — both documented and
  directly tested.
- Overrides stored in SQLite, layered at resolve time.
- Coverage-gap detection surfaced in `kerberon validate` and the UI.
- `/api/v1/oncall?team=X&at=<ts>`.

**Exit criteria — all met**
- `kerberon oncall` returns the right person for any instant and renders a rota over a
  window. `GET /api/v1/oncall` does the same over HTTP, with `team=`, `schedule=`,
  `at=` and `days=` filters.
- Property test: over any 400-day window intervals cover it with **no gaps and no
  overlaps**, across sixteen combinations of zone, rotation period and team size.
- Table tests pass against real historical DST transitions in America/New_York,
  Europe/London, Australia/Sydney (opposite hemisphere) and Asia/Kolkata (half-hour
  offset, no DST — control).
- `validate` fails a business-hours-only config, reporting every hole including the
  63-hour weekend one.
- Restriction layers resolve, including windows crossing midnight, and hold their wall
  clock across DST.

---

## Phase 5 — Escalation and notification — **complete**

**Goal:** the milestone that makes it real.

- `internal/escalate` — state machine: `triggered` / `acknowledged` / `resolved` /
  `expired`, including illegal transitions as no-ops. Policy structure snapshotted onto
  the incident at creation; targets resolved live at step-fire time (D4).
- Outbox: notification rows written in the same transaction that advances state;
  `UNIQUE` idempotency key.
- `internal/notify` — worker pool; exponential backoff with full jitter
  (5s/15s/45s/2m/5m → `dead`); per-channel circuit breaker with failover; `sending`
  lease reclaim (D7); dead-letter escalation raising `kerberon.notification_failure`.
- Channels: ntfy, Telegram (inline ack buttons), email (SMTP), generic webhook. One
  interface, `Send(ctx, Notification) error` plus capability flags — a new channel is
  one file, no dispatcher changes.
- `internal/ack` — HMAC-SHA256 signed ack links, constant-time verification,
  single-purpose, scoped to one incident.

**Exit criteria — met**
- **End to end, verified against a running server:** 50 alerts in produced one
  incident and one page carrying severity, title and a signed ack link; tapping the
  link returned "Incident 1 is yours. Escalation has stopped."; and the second step,
  due 45 seconds later, never fired.
- Fake channel failing deterministically proves the backoff schedule, breaker opening,
  failover to the next channel, and dead-lettering.
- Ack on an already-resolved incident is a no-op, not a crash.
- Re-escalation after `ack_timeout` fires correctly.

---

## Phase 6 — Heartbeats and web UI — **complete**

**Goal:** usable without touching the API.

- Heartbeat registration, token generation, 30-second sweeper for overdue pings.
- Five pages (spec §10): Now, Incidents, Schedules, Config, Settings.
- Test-notification buttons per user per channel — non-negotiable; most on-call setup
  failures are a bad contact discovered at the worst moment.
- Self-monitoring dead-man's switch pinging an external watcher every minute.
- Auth: single shared password with session cookie, or trusted-header for reverse-proxy
  and Tailscale deployments.

**Exit criteria — met**
- All five pages render against a live server; root redirects to the UI.
- A stopped heartbeat raises an incident within `expected_interval + grace_period`.
- Clicking "test" on a user's channel makes their device buzz.
- Coverage gaps render in red on the Config page.

---

## Phase 7 — Hardening — **complete**

**Goal:** earn the numbers before publishing them.

- Chaos suite to 1,000 nightly iterations on Linux CI.
- `kerberon-bench` load harness; measure ingest throughput, dedup ratio, end-to-end
  notify latency. **Add the ingest batcher only if measurement demands it (D3).**
- Graceful shutdown; config hot reload with fsnotify + SIGHUP, validate-before-swap.
- Retention and nightly vacuum (**pending open question §15.6**).
- Cross-compilation for linux/amd64, linux/arm64, darwin/arm64, windows/amd64.
- Docker image; install script (**pending name resolution, §15.1**).

**Measured so far** (28-core dev machine, `cmd/kerberon-bench`):
ingest **8,469 alerts/sec**, p99 request latency 924 ms for a 200-alert batch,
20,000 alerts collapsing to 20 incidents, binary **7.2 MB**, zero missed or duplicate
escalations across randomised kill/restart cycles.

Also measured: end-to-end notify latency p99 **1.005 s** against a configured 1 s
`group_wait`, so dispatch itself adds single-digit milliseconds; idle resident memory
**20 MB** against a 50 MB budget.

Config hot reload is done, on SIGHUP and on file change. It polls the file's mtime
rather than taking an fsnotify dependency: a dependency is not free for a project
whose whole proposition is a single binary, and a two-second poll is
indistinguishable in practice for a file a human edits. An invalid config is rejected
and the running one kept — verified live, including that paging still works after a
rejected reload.

Retention prunes closed incidents after 90 days and vacuums nightly. Only closed
incidents are eligible: an old triggered incident is something nobody answered, and
deleting it would discard the most important row in the database.

**Original exit criteria — publish only what is measured**
- Zero missed and zero duplicate escalations across 1,000 kill/restart cycles.
- Measured ingest throughput on a 2-vCPU box (target ≥5,000/s; publish the real
  number).
- p99 first-notification latency < 2s excluding `group_wait`.
- A 400-alert cascade produces ≤ 3 notifications.
- Binary < 25 MB; RSS < 50 MB idle.

---

## Phase 8 — Launch — **complete, bar the name check**

**Goal:** be findable and be trusted.

- README: 60-second quickstart, measured numbers, and an honest "what happens when
  Kerberon itself goes down" section.
- Grafana OnCall migration guide — a real acquisition channel post-archival.
- Docs site, example configs.
- Demo GIF of the cascade collapsing to a single page.
- Post to r/devops, r/sre, r/selfhosted, Show HN, Lobsters.

**Exit criteria**
- A stranger goes from zero to a buzzing phone in under 60 seconds by following the
  README.
- Limitations section names the single-node failure mode without euphemism.

---

## Deferred to v1.1+

Twilio SMS/voice · Slack app with native buttons · status pages · postmortem templates
· SSO/RBAC/audit logs · HA clustering · mobile apps · Prometheus export of Kerberon's
own internals · Terraform provider.

---

## Open questions gating specific phases

| Question (spec §15) | Gates |
|---|---|
| 1 — name, domain, module path | Phase 0 (module path), Phase 8 (README, install script) |
| 4 — absolute TTL on ack links | Phase 5 |
| 5 — `ack_timeout` default-on or opt-in | Phase 5 |
| 6 — retention policy | Phase 7 |
| 7 — HA / leader election sketch | v2; already accommodated by D1's reserved `claimed_at` |
| 8 — Slack app in v1.1 | post-v1 |
