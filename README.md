# Kerberon

**On-call and paging in a single binary. No Postgres. No Redis. No RabbitMQ. No SaaS.**

*Three heads, three tiers. Primary doesn't answer, secondary does. Secondary doesn't
answer, everyone does.*

---

Kerberon turns a firing alert into a human being who is awake and looking at it.

It receives alerts from Alertmanager, Grafana or any webhook, collapses related ones
into a single incident, works out who is on call for the responsible team right now,
and pages them — escalating until somebody acknowledges.

That is the whole product. It does not collect metrics, store logs, draw dashboards or
evaluate alerting rules. Prometheus and Grafana already do those well. Kerberon is the
last mile.

## Sixty seconds

```bash
curl -L https://kerberon.sh/install | sh
```

Write a `kerberon.yaml` — start from [`examples/kerberon.yaml`](examples/kerberon.yaml):

```yaml
server:
  listen: "0.0.0.0:8080"
  external_url: "https://kerberon.example.com"   # ack links are built from this
  secret_key: "${KERBERON_SECRET}"
  ingest_token: "${KERBERON_INGEST_TOKEN}"

users:
  - id: sarthak
    timezone: "Asia/Kolkata"
    contacts:
      ntfy: "https://ntfy.sh/kerberon-sarthak-a8f3"

teams:
  - name: platform
    members: [sarthak]

schedules:
  - name: platform-primary
    team: platform
    timezone: "Asia/Kolkata"
    layers:
      - name: base-weekly
        type: rotation
        participants: [sarthak]
        rotation: weekly
        handoff: {day: monday, time: "09:00"}

escalation_policies:
  - name: critical-24x7
    steps:
      - {delay: 0,  targets: [schedule:platform-primary], channels: [ntfy]}
      - {delay: 5m, targets: [team:platform],             channels: [ntfy]}

routes:
  - match: {severity: critical}
    team: platform
    policy: critical-24x7
    group_by: [alertname, cluster]

channels:
  ntfy:
    default_server: "https://ntfy.sh"
```

Then:

```bash
kerberon validate --config kerberon.yaml
kerberon serve --config kerberon.yaml
```

Point Alertmanager at `POST /api/v1/alertmanager` with the ingest token as a bearer
header. Your phone buzzes. Tap the link. Escalation stops.

## Why this exists

Grafana OnCall OSS — the most feature-complete open-source PagerDuty alternative —
entered maintenance in March 2025 and was archived in March 2026. Its OSS build relied
on Grafana Cloud as a relay for SMS, voice and push, and that relay went at the same
time.

What is left is heavy. Running a self-hosted pager typically means operating Postgres,
Redis, RabbitMQ and a Twilio account. Kerberon is not competing with PagerDuty on
features. It competes with *"we don't have a paging system, because standing one up is
a project."*

## What it does

**Alert fatigue is the design constraint.** A bad deploy that fires four hundred alerts
should wake one person once.

- **Fingerprinting** ignores volatile labels, so a rescheduled Kubernetes pod is not a
  new alert. That single detail is the most common cause of duplicate paging.
- **Grouping** collapses related alerts into one incident, enforced by a database
  index rather than application-layer hope.
- **`group_wait`** holds the first page briefly so a cascade arrives and the
  notification says "12 services down" rather than paging twelve times.

**Escalation that survives a restart.** Every future action — escalate, expire, close a
grace window — is a row in SQLite, not an in-memory timer. Process restarts correlate
with exactly the incidents that need paging, so losing a pending escalation on restart
is not an acceptable failure mode.

**Correct answers about time.** Rotations are wall-clock times in named IANA zones.
A week containing a daylight-saving transition is 167 or 169 hours, and adding
`7 * 24h` silently opens a coverage gap twice a year.

**It tells you when it can't page you.** Per-channel circuit breakers, dead-lettering,
and a loud `PAGE NOT DELIVERED` when delivery fails outright.

## Measured, not claimed

Every number below was produced by [`cmd/kerberon-bench`](cmd/kerberon-bench) or by the
test suite on a 28-core development machine. Reproduce them yourself:

```bash
kerberon-bench --url http://127.0.0.1:8080 --token "$KERBERON_INGEST_TOKEN" \
  --alerts 20000 --batch 200 --concurrency 8 --groups 20
```

| | Measured | Spec target |
|---|---|---|
| Ingest throughput | **8,469 alerts/sec** | ≥ 5,000/sec |
| Request latency (200-alert batch) | p50 127 ms · p99 924 ms | — |
| Cascade collapse | **20,000 alerts → 20 incidents** | ≤ 3 pages per cascade |
| End-to-end notify latency | **p99 1.005 s** with a 1 s `group_wait` configured, so dispatch adds single-digit ms | < 2 s excluding `group_wait` |
| Idle resident memory | **20 MB** | < 50 MB |
| Binary size | **7.2 MB** | < 25 MB |
| Missed or duplicate escalations | **zero**, across randomised kill/restart cycles | zero |

The throughput figure is honest about its history: the first measurement was
1,492 alerts/sec, and the benchmark exposed a quadratic dedup query that no unit test
could have caught. See `docs/DECISIONS.md` and the commit history.

All of these come from a 28-core Windows development machine, which is not a $5 VPS.
Treat them as an upper bound on what this code can do, not a promise about your
hardware — and re-run the benchmark on yours before quoting anything.

## What happens when Kerberon itself goes down

v1 runs on one node. If that node is down, pages are not delivered. This is a real
limitation and pretending otherwise would be worse than stating it.

Mitigate it:

- **Run a dead-man's switch on Kerberon itself.** Have it ping an external free
  watcher (healthchecks.io, or a second Kerberon) every minute. If Kerberon dies, that
  watcher pages the team. It costs nothing and it is the honest answer.
- **Deploy it away from what it monitors.** A pager running inside the cluster it
  watches is not a pager.

Clustering is a v2 concern; it needs leader election to replace the single-writer
scheduler.

## Also honest about

- **No SMS or voice in v1.** Twilio needs an account and a credit card, which blocks
  anyone trying the thing. ntfy at maximum priority bypasses Do Not Disturb on Android;
  iOS is weaker. Judge whether that clears your bar before depending on it.
- **No SSO, RBAC or audit logs.** Deliberately v2+. If you need them, Kerberon v1 is
  not for you.
- **Delivery is at-least-once.** After a crash mid-send you may get a duplicate page.
  A duplicate is an annoyance; a missed page is a failure.
- **Ack links are bearer capabilities.** Anyone holding one can acknowledge that one
  incident, and the act is loudly visible in the timeline. Requiring a login at 3am is
  how incidents go unacknowledged.

## Channels

| Channel | Status | Notes |
|---|---|---|
| **ntfy** | ✅ | Free, self-hostable, push to phone. Max priority bypasses DND. The default. |
| **Telegram** | ✅ | Free, renders a real inline Acknowledge button. |
| **Email (SMTP)** | ✅ | Universal fallback. Quiet — put it late in a policy. |
| **Generic webhook** | ✅ | Slack, Discord, anything. Payload is Slack-compatible. |
| **Twilio SMS/voice** | ⏸ v1.1 | Deferred: needs an account and a card, which blocks the demo. |

## Commands

```
kerberon serve      Run the server
kerberon validate   Check a config and report every problem, with line numbers
kerberon migrate    Create or upgrade the database schema
kerberon oncall     Print who is on call, or a rota over a window
kerberon version    Version information
```

Configuration is hot-reloaded on `SIGHUP` and when the file changes on disk. An
invalid config **never** replaces a running one — a typo taking paging offline would
be far worse than briefly running on a stale config. The listen address, database path
and secret key still need a restart, and the log says so rather than pretending
otherwise.

Closed incidents are pruned after 90 days and the database vacuumed nightly; SQLite
does not shrink a file on `DELETE`, so without that a pruned year still occupies a
year of disk.

`validate` is meant to run in CI. It fails on unknown references, invalid timezones,
unset `${VAR}`s, a user paged on a channel they have no address for, and **coverage
gaps** — so a hole in the rotation fails a pull request instead of failing at 3am.

## API

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/v1/alerts` | Kerberon's own JSON format |
| `POST` | `/api/v1/alertmanager` | Prometheus Alertmanager webhook |
| `POST` | `/api/v1/grafana` | Grafana unified alerting webhook |
| `GET`/`POST` | `/api/v1/heartbeat/{token}` | Dead-man's switch ping |
| `GET` | `/api/v1/oncall` | Who is on call — `team=`, `schedule=`, `at=`, `days=` |
| `POST` | `/api/v1/incidents/{id}/ack` | Acknowledge |
| `POST` | `/api/v1/incidents/{id}/resolve` | Resolve |
| `GET` | `/ui/` | Web UI |

## Web UI

Five server-rendered pages, embedded in the binary. No build step, no `node_modules`.

**Now** (who is on call, what is open, heartbeat health) · **Incidents** (with full
timelines) · **Schedules** (30-day rota with gaps shown inline) · **Config**
(read-only, gaps highlighted) · **Settings** (test-notification buttons).

The test buttons matter: most on-call setup failures are a wrong contact discovered at
the worst possible moment. Confirm your phone actually buzzes before an incident
depends on it.

## Docker

```bash
docker run -v ./kerberon.yaml:/etc/kerberon/kerberon.yaml \
           -v kerberon-data:/data -p 8080:8080 \
           ghcr.io/sarthak-47/kerberon:latest
```

Built `FROM scratch` — the binary is static, so there is no base image to patch.

## Building

Kerberon pins its own Go toolchain and keeps every cache inside the project folder, so
building it will not touch your system Go.

```bash
source ./scripts/env.sh      # or . .\scripts\env.ps1 on Windows
go build ./...
go test ./...
```

See [`CONTRIBUTING.md`](CONTRIBUTING.md). Contributions need a DCO sign-off
(`git commit -s`).

## Documentation

- [`docs/DECISIONS.md`](docs/DECISIONS.md) — architecture decisions, including the four
  that amend the original spec and why
- [`docs/ROADMAP.md`](docs/ROADMAP.md) — phased build plan and what each phase actually
  demonstrated
- [`docs/MIGRATING-FROM-GRAFANA-ONCALL.md`](docs/MIGRATING-FROM-GRAFANA-ONCALL.md) —
  concept mapping, a cutover plan, and a frank list of what does not carry over
- [`examples/kerberon.yaml`](examples/kerberon.yaml) — a complete, commented config

## License

Apache-2.0. Every feature described here is free, forever.

**No telemetry, ever, by default.** Kerberon makes no outbound connection you have not
configured.
