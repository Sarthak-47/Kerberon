# Kerberon development environment.
#
# Dot-source this before running any `go` command:
#
#     . .\scripts\env.ps1
#
# It pins the project-local Go toolchain and redirects every Go cache and config
# path into the project folder, so nothing is written outside it (CLAUDE.md R1/R2).
#
# Toolchain: go1.26.6 windows/amd64
# SHA256:    5b6c5b556525810463b5c897b50dc7a82d6a3dc0bfaf55d990a7e9f31d6b2318
# Source:    https://go.dev/dl/go1.26.6.windows-amd64.zip

$ErrorActionPreference = 'Stop'

$Root = Split-Path -Parent $PSScriptRoot

if (-not (Test-Path "$Root\.toolchain\go\bin\go.exe")) {
    Write-Error "Go toolchain missing at $Root\.toolchain\go. See docs/ROADMAP.md Phase 0."
}

# --- Toolchain -------------------------------------------------------------
$env:GOROOT = "$Root\.toolchain\go"

# Put our Go first on PATH, and drop any other Go that may be present so a
# machine-wide install cannot silently shadow the pinned one.
$clean = ($env:PATH -split ';') | Where-Object {
    $_ -and ($_ -notmatch '[\\/]go[\\/]bin[\\/]?$')
}
$env:PATH = (@("$env:GOROOT\bin", "$Root\.gopath\bin") + $clean) -join ';'

# --- Project-local caches --------------------------------------------------
$env:GOPATH     = "$Root\.gopath"
$env:GOMODCACHE = "$Root\.gopath\pkg\mod"
$env:GOBIN      = "$Root\.gopath\bin"
$env:GOCACHE    = "$Root\.gocache"

# Go writes build temp files to the system temp dir, and `go env -w` writes to
# %AppData%\go\env. Both would escape the project folder.
$env:GOTMPDIR = "$Root\.tmp"
$env:GOENV    = "$Root\.gopath\env"

# Never fetch a different toolchain version than the pinned one.
$env:GOTOOLCHAIN = 'local'

foreach ($d in @($env:GOPATH, $env:GOMODCACHE, $env:GOBIN, $env:GOCACHE, $env:GOTMPDIR)) {
    New-Item -ItemType Directory -Force -Path $d | Out-Null
}

# --- Telemetry -------------------------------------------------------------
# GOTELEMETRY and GOTELEMETRYDIR are NON-SETTABLE: `go env` reports them but
# setting them as environment variables does nothing. The only control is
# `go telemetry off`, and the mode file lives under os.UserConfigDir()
# (%AppData%\go on Windows), which is outside the project folder.
#
# In 'local' mode the toolchain writes counter files there on every build,
# breaching R1. Warn loudly rather than failing silently.
$telemetryMode = (& go telemetry) 2>$null
if ($telemetryMode -and $telemetryMode.Trim() -ne 'off') {
    Write-Warning "Go telemetry is '$($telemetryMode.Trim())' - the toolchain writes counter files to"
    Write-Warning "  $(& go env GOTELEMETRYDIR)"
    Write-Warning "which is outside the project folder (CLAUDE.md R1). Disable it with:  go telemetry off"
}

Write-Host "Kerberon env ready" -ForegroundColor Green
Write-Host "  go       $(& go version)"
Write-Host "  GOROOT   $env:GOROOT"
Write-Host "  GOPATH   $env:GOPATH"
Write-Host "  GOCACHE  $env:GOCACHE"
