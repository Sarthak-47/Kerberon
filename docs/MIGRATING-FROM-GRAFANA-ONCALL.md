# Migrating from Grafana OnCall

Grafana OnCall OSS entered maintenance in March 2025 and was archived in March 2026.
Its OSS build relied on Grafana Cloud as a relay for SMS, voice and push, and that
relay was deprecated at the same time. If you are reading this, you probably need
somewhere to go.

This guide maps OnCall concepts onto Kerberon, and is honest about what does not map.

## Read this first: what you lose

Kerberon is a smaller product than OnCall, deliberately. Before migrating, check that
none of these is load-bearing for you:

| OnCall had | Kerberon v1 |
|---|---|
| Web UI for editing schedules and policies | **Config file only.** Schedules are code, reviewed in a PR. Overrides are the one exception and are editable at runtime. |
| Grafana SSO / RBAC | **Not present.** Single shared password or trusted-header auth behind a reverse proxy. Deliberate v2+. |
| SMS and voice via the Cloud relay | **Not in v1.** Twilio slots in at v1.1. ntfy at maximum priority bypasses Do Not Disturb on Android; iOS is weaker. |
| Slack app with native interactive buttons | **Generic webhook only.** A real Slack app needs OAuth and a public callback, which conflicts with the zero-configuration promise. |
| Mobile apps | **None.** Notifications ride on ntfy or Telegram, which you already have. |
| Multiple organisations | **Single-org.** Run two instances. |
| Alert grouping templates in Jinja | **Label-based grouping only.** `group_by` on label names. |

If SSO or SMS is non-negotiable, Kerberon v1 is not your answer and it is better to
know that now than after a migration.

## What you gain

- One binary and one SQLite file. No Postgres, no Redis, no RabbitMQ, no Celery
  workers.
- Schedules reviewable in a pull request, with `kerberon validate` failing CI on a
  coverage gap.
- No cloud relay to be deprecated out from under you.

## Concept mapping

| Grafana OnCall | Kerberon | Notes |
|---|---|---|
| Integration | Route + ingest endpoint | One endpoint per source format; routing is by label match. |
| Escalation Chain | `escalation_policies` | Steps with delays and targets. |
| Schedule (web or iCal) | `schedules` with layers | Rotations and restriction windows in YAML. |
| On-call shift | Layer | `rotation: weekly` plus a `handoff`. |
| Override / shift swap | `overrides` (runtime) | Created through the API; not in config, so no commit needed for a swap. |
| Route (Jinja) | `routes[].match` | Exact label equality. Regex is deliberately not supported: a regex that fails to match is a silent non-page. |
| Grouping template | `group_by` | List of label names. |
| Notification policy per user | Channels per escalation step | Kerberon attaches channels to steps, not to users. |
| Maintenance mode | *Not present* | Silence at Alertmanager, which already does it well. |
| Heartbeat | `heartbeats` | Same idea: a ping that must arrive, or an incident. |

## Step by step

### 1. Point Alertmanager somewhere new

If OnCall was receiving Alertmanager webhooks, the change is one URL and one header.

```yaml
# alertmanager.yml
receivers:
  - name: kerberon
    webhook_configs:
      - url: https://kerberon.example.com/api/v1/alertmanager
        http_config:
          authorization:
            type: Bearer
            credentials: <your KERBERON_INGEST_TOKEN>
```

Kerberon accepts the Alertmanager v4 webhook format unchanged, so no payload
transformation is needed.

Grafana unified alerting sends the same shape; point it at `/api/v1/grafana` so the
source is recorded correctly.

### 2. Translate your escalation chain

An OnCall chain of "notify on-call, wait 5 minutes, notify next, wait 5 minutes,
notify the whole team" becomes:

```yaml
escalation_policies:
  - name: critical-24x7
    repeat: 2
    ack_timeout: 30m
    steps:
      - {delay: 0,   targets: [schedule:platform-primary],   channels: [ntfy, telegram]}
      - {delay: 5m,  targets: [schedule:platform-secondary], channels: [ntfy, telegram]}
      - {delay: 10m, targets: [team:platform],               channels: [ntfy, telegram]}
```

Two differences worth knowing:

- **`delay` is measured from the previous step**, not from when the incident opened.
- **`ack_timeout` resumes escalation** if an acknowledged incident is not resolved in
  time. OnCall had no direct equivalent. It catches "acknowledged and fell back
  asleep", which is a real failure mode. Set it to `0` to disable.

### 3. Translate your schedules

A weekly rotation handing over Monday at 09:00:

```yaml
schedules:
  - name: platform-primary
    team: platform
    timezone: "Asia/Kolkata"      # an IANA name, never a fixed offset
    layers:
      - name: base-weekly
        type: rotation
        participants: [sarthak, priya]
        rotation: weekly
        handoff: {day: monday, time: "09:00"}
```

Business hours with after-hours cover behind it:

```yaml
    layers:
      - name: business-hours
        type: restriction
        participants: [sarthak, priya]
        rotation: weekly
        handoff: {day: monday, time: "09:00"}
        restriction:
          days: [monday, tuesday, wednesday, thursday, friday]
          start: "09:00"
          end: "18:00"
      - name: after-hours          # listed second, so it covers what the first does not
        type: rotation
        participants: [arun, priya]
        rotation: weekly
        handoff: {day: monday, time: "09:00"}
```

Layers are evaluated top-down and the first that produces somebody wins.

**iCal schedules do not import.** OnCall could read a calendar; Kerberon cannot. If
your rotation lives in Google Calendar you will need to express it as layers, or keep
using overrides for the irregular parts.

### 4. Check the rotation before trusting it

This is the step that repays itself:

```bash
kerberon validate --config kerberon.yaml
```

It fails on unknown references, invalid timezones, unset `${VAR}`s, a user paged on a
channel they have no address for, and **coverage gaps over the next 400 days**. A
business-hours-only schedule with nothing behind it fails here rather than at 3am.

Then look at the rota directly:

```bash
kerberon oncall --config kerberon.yaml --days 30
```

### 5. Move heartbeats

OnCall heartbeats become:

```yaml
heartbeats:
  - name: nightly-backup
    expected_interval: 24h
    grace_period: 1h
    team: platform
    severity: critical
```

On first start Kerberon mints a token and logs the ping URL **once**. Capture it then;
it is not recoverable afterwards by design. Update the `curl` in your crontab to the
new URL.

### 6. Prove your phone actually buzzes

Open `/ui/settings` and press the test button for each contact. Most on-call setup
failures are a wrong contact discovered at the worst possible moment, and this is
thirty seconds of work against that.

## Running both during a cutover

Nothing stops Alertmanager sending to OnCall and Kerberon at once. Add Kerberon as a
second receiver, run them in parallel for a week, and compare what each paged for.
Two pages per incident is annoying; a missed page during a migration is worse.

```yaml
route:
  routes:
    - receiver: oncall
      continue: true      # keep going after this one matches
    - receiver: kerberon
```

## What to do about the single-node question

OnCall ran on infrastructure you were already operating. Kerberon v1 is one process,
and if that process is down, pages are not delivered.

Before you cut over, set up the dead-man's switch:

- Have Kerberon ping an external watcher every minute — healthchecks.io is free, or a
  second Kerberon instance works.
- Run it somewhere other than the cluster it watches. A pager inside the thing it
  monitors is not a pager.

This is the honest mitigation, and it costs nothing.
