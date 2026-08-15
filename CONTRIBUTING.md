# Contributing to Kerberon

Thanks for considering a contribution. Kerberon is deliberately small — please read
the non-goals in the spec before proposing a feature, since scope creep is the
project's primary failure mode.

## Developer Certificate of Origin

All commits must be signed off. This certifies you wrote the patch or otherwise have
the right to submit it under Apache-2.0. It is not a copyright assignment.

```bash
git commit -s -m "your message"
```

That appends a trailer:

```
Signed-off-by: Your Name <your.email@example.com>
```

The name and email must be real and must match your commit author. A CI check enforces
this on every commit in a pull request. To fix an existing branch:

```bash
git rebase --signoff main
```

The full DCO text is at <https://developercertificate.org/>.

## Development environment

Kerberon pins its own Go toolchain and keeps every cache inside the project folder, so
building it will not touch your machine's global Go installation.

```powershell
# Windows
. .\scripts\env.ps1
go build ./...
go test ./...
```

```bash
# Linux / macOS
source ./scripts/env.sh
go build ./...
go test ./...
```

If `.toolchain/` is absent, the env script will tell you; see `docs/ROADMAP.md`
Phase 0.

**`go test -race` needs a C compiler.** The race detector is the one place CGO is
required, and Windows has no C toolchain by default, so `-race` fails locally there
with a `runtime/cgo` build error. Run the plain suite on Windows; Linux CI runs every
test under `-race` on each push. The same applies to the chaos suite, which is
Linux-only for a different reason — Windows has no faithful `SIGKILL`.

## House rules for code

These are enforced by CI and are not negotiable — they exist because the project's
correctness claims depend on them.

**Nothing outside `internal/clock` may touch the clock.** No `time.Now`, `time.Sleep`,
`time.After`, `time.NewTimer`, `time.NewTicker`, or `time.Tick`. Every subsystem takes
a `Clock`. This is what makes the escalation engine and schedule resolver testable, and
a violation silently makes tests nondeterministic rather than failing loudly.

**No `time.Sleep` in tests.** Use `FakeClock.Advance`. A test that sleeps is a test
that will flake in CI.

**No CGO.** SQLite access goes through `modernc.org/sqlite`. A CGO dependency breaks
static cross-compilation, which is the entire product differentiator.

**Timer handlers may not perform I/O.** A timer's effect must be a pure database state
change applied in a single transaction — that is what makes exactly-once execution fall
out of SQLite's atomicity rather than application logic. Notifications go to the outbox
table; dispatch workers send them. See `docs/DECISIONS.md` D1.

**No new required external service.** If a change would make Postgres, Redis, a broker,
or any network service mandatory to run Kerberon, it will be rejected regardless of
merit.

## Tests

New behaviour needs tests. In particular:

- Schedule changes need table tests against real historical DST transitions.
- Escalation changes need state-machine tests, including illegal transitions — an ack
  on a resolved incident must be a no-op, not a crash.
- Timer changes must keep the chaos suite passing. It runs on Linux only; `SIGKILL`
  has no faithful Windows equivalent.

## Pull requests

- One logical change per PR.
- Explain the *why* in the description; the diff already shows the what.
- If you change behaviour described in the spec or in `docs/DECISIONS.md`, say so
  explicitly and update the document in the same PR.

## Security

Do not open a public issue for a security problem. See `SECURITY.md`.
