# Kerberon — Roadmap

Derived from spec §12, with the week 2/3 swap from `DECISIONS.md` D9 applied
(durable timers precede ingest, because `group_wait` is itself a durable timer).

Every phase ends in something runnable and demonstrable. No phase is pure refactoring.
A phase is complete only when its **exit criteria** are demonstrated, not when the code
is written.

---

## Phase 0 — Prerequisites

Not in the spec's eight weeks. Must be settled before any Go code is written.

| Item | State |
|---|---|
| Go 1.23+ toolchain installed | **Blocked** — not present on the machine |
| Project-local `GOPATH`/`GOMODCACHE`/`GOCACHE` (`scripts\env.ps1`) | Pending Go |
| Module path decided (depends on name — spec §15.1) | **Open question** |
| `git init`, per-repo identity `Sarthak-47 <0906sarthak@gmail.com>` | Pending |
| `.gitignore` covering `.gopath/`, `.gocache/`, `.tmp/`, `*.db*` | Pending |
| CI workflows written into `.github/workflows/` | Pending |
| `LICENSE` (Apache-2.0), `CONTRIBUTING.md` (DCO), `SECURITY.md` | Pending |

**Exit:** `.\scripts\env.ps1; go version` works, module builds, `git log` shows the
correct author with no co-author trailer.

---

## Phase 1 — Skeleton

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

**Exit criteria**
- `kerberon validate --config examples/kerberon.yaml` passes on a good config and
  fails with a precise, line-referenced message on each of a set of broken ones.
- `kerberon migrate` creates the schema; re-running is a no-op.
- `FakeClock` unit tests pass with zero `time.Sleep` anywhere in the test suite.
- CI green on all four target platforms.

**Unblocks:** everything.

---

## Phase 2 — Durable timers *(moved ahead of ingest, D9)*

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

**Exit criteria**
- Schedule a timer, `kill -9` the process, restart, timer fires **exactly once**.
- Cancellation races are covered: cancel-during-fire never double-executes.
- A process down for ten simulated minutes catches up on restart in `fire_at` order.
- Chaos harness runs on Linux (WSL/Docker) — documented as not runnable on Windows.

**Unblocks:** grouping (`group_wait`), escalation, heartbeats.

---

## Phase 3 — Ingest, dedup, grouping

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

**Exit criteria**
- POST a recorded alert storm; incidents form and collapse per golden-file expectations.
- A group re-firing inside `resolve_grace` cancels the resolve and does not re-page.
- Fixture payloads from real Alertmanager and Grafana instances parse correctly.
- Concurrent alerts for one new group produce exactly one incident.

---

## Phase 4 — Schedules

**Goal:** correct answers about time, including twice a year when it is hard.

- Layer model: overrides → restrictions → base rotation, resolved highest-first.
- `Intervals(from, to)` as the primitive; `At(t)` derived from it (D6).
- IANA timezones, `time/tzdata` embedded; DST policies — nonexistent local times shift
  forward, ambiguous local times take the first occurrence — both documented and
  directly tested.
- Overrides stored in SQLite, layered at resolve time.
- Coverage-gap detection surfaced in `kerberon validate` and the UI.
- `/api/v1/oncall?team=X&at=<ts>`.

**Exit criteria**
- `kerberon oncall --team platform --at <ts>` returns the right person for any instant.
- Property test: over any 400-day window, intervals cover it with **no gaps and no
  overlaps**.
- Table tests pass against real historical DST transitions in America/New_York,
  Europe/London, Australia/Sydney (opposite hemisphere) and Asia/Kolkata (half-hour
  offset, no DST — control).
- `validate` fails a config containing a deliberate coverage gap.

---

## Phase 5 — Escalation and notification

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

**Exit criteria**
- **End to end: alert in → phone buzzes → tap to acknowledge → escalation stops.**
- Fake channel failing deterministically proves the backoff schedule, breaker opening,
  failover to the next channel, and dead-lettering.
- Ack on an already-resolved incident is a no-op, not a crash.
- Re-escalation after `ack_timeout` fires correctly.

---

## Phase 6 — Heartbeats and web UI

**Goal:** usable without touching the API.

- Heartbeat registration, token generation, 30-second sweeper for overdue pings.
- Five pages (spec §10): Now, Incidents, Schedules, Config, Settings.
- Test-notification buttons per user per channel — non-negotiable; most on-call setup
  failures are a bad contact discovered at the worst moment.
- Self-monitoring dead-man's switch pinging an external watcher every minute.
- Auth: single shared password with session cookie, or trusted-header for reverse-proxy
  and Tailscale deployments.

**Exit criteria**
- Full incident lifecycle driven entirely from the UI.
- A stopped heartbeat raises an incident within `expected_interval + grace_period`.
- Clicking "test" on a user's channel makes their device buzz.
- Coverage gaps render in red on the Config page.

---

## Phase 7 — Hardening

**Goal:** earn the numbers before publishing them.

- Chaos suite to 1,000 nightly iterations on Linux CI.
- `kerberon-bench` load harness; measure ingest throughput, dedup ratio, end-to-end
  notify latency. **Add the ingest batcher only if measurement demands it (D3).**
- Graceful shutdown; config hot reload with fsnotify + SIGHUP, validate-before-swap.
- Retention and nightly vacuum (**pending open question §15.6**).
- Cross-compilation for linux/amd64, linux/arm64, darwin/arm64, windows/amd64.
- Docker image; install script (**pending name resolution, §15.1**).

**Exit criteria — publish only what is measured**
- Zero missed and zero duplicate escalations across 1,000 kill/restart cycles.
- Measured ingest throughput on a 2-vCPU box (target ≥5,000/s; publish the real
  number).
- p99 first-notification latency < 2s excluding `group_wait`.
- A 400-alert cascade produces ≤ 3 notifications.
- Binary < 25 MB; RSS < 50 MB idle.

---

## Phase 8 — Launch

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
