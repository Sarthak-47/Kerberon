# Security Policy

Kerberon holds notification credentials and sits on the critical path for incident
response. A failure here can mean a page that never arrives. Reports are taken
seriously.

## Reporting a vulnerability

**Do not open a public issue.**

Email **0906sarthak@gmail.com** with:

- a description of the issue and its impact,
- steps to reproduce, or a proof of concept,
- the Kerberon version and platform,
- whether the issue is already public anywhere.

You will get an acknowledgement within 72 hours and an assessment within 7 days.
Coordinated disclosure is preferred; credit is given in the release notes unless you
ask otherwise.

## Supported versions

Pre-1.0. Only the latest release receives fixes.

## Areas of particular interest

If you are looking for somewhere to start, these carry the most risk:

- **Ack token verification** — HMAC-SHA256, truncated to 16 bytes, base64url. Must be
  constant-time, single-purpose, and scoped to one incident. A forgery silences a page.
- **Ingest authentication** — bearer token comparison must be constant-time.
- **Config secret handling** — `${VAR}` expansion must never leak resolved credentials
  into logs, the Config UI page, or error messages.
- **Web UI auth** — v1 uses a single shared password or trusted-header auth behind a
  reverse proxy. Session handling and header-trust logic are worth scrutiny.
- **Webhook payload parsing** — ingest handlers accept untrusted input from the network.

## Known limitations, by design

These are documented trade-offs rather than vulnerabilities, and are not eligible for
report:

- **Single-node.** If the host is down, pages are not delivered. Mitigated by an
  external dead-man's switch; see the README.
- **No SSO, RBAC, or audit logs in v1.** Deliberately deferred. Kerberon v1 is not
  built for environments that require them.
- **Ack links are bearer capabilities.** Anyone holding a link can acknowledge that one
  incident. The blast radius is deliberately tiny and the action is loudly visible in
  the incident timeline. Requiring a login to acknowledge at 3am is how incidents go
  unacknowledged.
- **At-least-once notification delivery.** After a crash mid-send, a notification may be
  delivered twice. A duplicate page is preferable to a missed one.
