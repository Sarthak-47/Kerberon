# Kerberon — working rules

These rules are set by the project owner and override default behaviour. They apply to
every session. Read them before taking any action.

---

## R1. Nothing leaves the project folder

Every file this project creates — source, build output, dependency caches, temporary
files, test databases, benchmark results, scratch work — lives under
`D:\Claude Code Projs\Kerberon`.

- No writing to `%TEMP%`, `%USERPROFILE%`, `%APPDATA%`, or any system location.
- No global tool installs as a side effect of project work.
- Temporary and intermediate files go in `./.tmp/` (git-ignored), never a system temp
  directory.
- Reading from outside the folder is permitted (e.g. the spec document in
  `Downloads`). The rule governs **writes**.

## R2. Dependencies are isolated to the project folder

Go has no `venv`. The equivalent isolation is achieved by relocating the Go caches
into the project and vendoring dependencies:

```
GOPATH      = <project>\.gopath
GOMODCACHE  = <project>\.gopath\pkg\mod
GOBIN       = <project>\.gopath\bin
GOCACHE     = <project>\.gocache
```

- `scripts\env.ps1` sets these for a shell session. Run it before any `go` command.
- Dependencies are vendored (`go mod vendor`) and `./vendor` is committed, so builds
  are reproducible and work offline. Build with `-mod=vendor`.
- `.gopath/`, `.gocache/`, and `.tmp/` are git-ignored.
- Nothing is ever installed with `go install` to a global path.

### The one documented exception to R1

Go's toolchain telemetry cannot be redirected. `GOTELEMETRY` and `GOTELEMETRYDIR`
are **non-settable** — `go env` reports them, but exporting them has no effect. The
data path is derived from `os.UserConfigDir()`, which is `%AppData%\go` on Windows.

In the default `local` mode the toolchain writes counter files there on every build.
Telemetry is therefore set to `off` machine-wide:

```
go telemetry off
```

That leaves a single 14-byte mode file at `%AppData%\go\telemetry\mode` — the setting
that keeps it off — plus ~16 KB of counter files written before it was disabled. No
further writes occur. `scripts\env.ps1` and `scripts\env.sh` warn on session start if
the mode is ever anything other than `off`, so a regression is caught immediately
rather than leaking silently.

This also satisfies the spec's "no telemetry, ever" position (§13).

## R3. Git identity and attribution

All commits and pushes use **exactly** this identity:

```
Sarthak-47 <0906sarthak@gmail.com>
```

Set per-repository, never globally:

```
git config user.name  "Sarthak-47"
git config user.email "0906sarthak@gmail.com"
```

**No co-author trailers of any kind. No `Co-Authored-By: Claude`. No "Generated with
Claude Code" in commit messages, PR titles, or PR bodies.** This overrides the default
Claude Code commit convention. Commits are authored by Sarthak-47 and nobody else.

Commits also carry DCO sign-off (`git commit -s`), per spec §13 — the `Signed-off-by`
line will read `Sarthak-47 <0906sarthak@gmail.com>`, which is correct and is not a
co-author trailer.

## R4. Push only to a repository the owner provides

Do not create GitHub repositories, do not push to a remote that was not explicitly
given, and do not add remotes speculatively. Wait for the owner to supply the repo URL.

**Authorized remote** (provided by the owner, 2026-08-15):

```
origin  https://github.com/Sarthak-47/Kerberon.git
```

Commit and push as work progresses. No other remote may be added.

Note: the repository is `Kerberon` but the Go module path is
`github.com/Sarthak-47/kerberon`, lowercase per Go convention. GitHub resolves
repository names case-insensitively so `go get` works, and the all-lowercase path
avoids the case-sensitivity problems that bit `Sirupsen/logrus`. If the repository is
ever renamed to lowercase, no code change is needed.

## R5. CI/CD

A CI pipeline is required (see `docs/ROADMAP.md`, Phase 0). Workflow files are written
into `.github/workflows/` ahead of time; they activate when the repo is first pushed.
CI must enforce, at minimum: build on four target platforms, `go vet`, tests with race
detection, the `time.Now()` lint rule (DECISIONS D5), and DCO sign-off.

## R6. Ask before acting when anything is unclear

If a request is ambiguous, if a rule here conflicts with the task, or if an action
would touch anything outside this folder — stop and ask. Do not pick a reasonable
default and proceed.

This applies especially to: installing software, adding a dependency, anything
network-facing, anything that writes outside the project folder, and any git operation
beyond local commits.

---

## Project reference

- **Spec:** `C:\Users\Sarthak Singh\Downloads\KERBERON_PROJECT_SPEC.md` (v1.0) — the
  founding document. Where it states a decision, treat it as decided; surface conflicts
  rather than substituting a different design.
- **Decisions:** `docs/DECISIONS.md` — architecture decisions taken on top of the spec,
  including the four that amend it.
- **Roadmap:** `docs/ROADMAP.md` — phased build plan and exit criteria.

## Hard technical constraints (from spec, non-negotiable)

- Single static binary. No CGO. `modernc.org/sqlite` only.
- No required external service — no Postgres, Redis, RabbitMQ, NATS, or Kubernetes.
- Nothing outside `internal/clock` may call `time.Now`, `time.Sleep`, `time.After`,
  `time.NewTimer`, `time.NewTicker`, or `time.Tick`.
- Timezones are IANA names, never fixed offsets. `import _ "time/tzdata"`.
- Timer effects are database-only, applied in a single transaction (DECISIONS D1).
- No telemetry, ever, by default.
