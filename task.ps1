#!/usr/bin/env pwsh
# RunMesh task runner for Windows (GNU make equivalent).
#   ./task.ps1 build | run | test | test-pg | test-integration | race | cover
#                 | bench | load | vet | fmt | lint | check | tidy | clean
#                 | db-up | db-down | db-reset | monitoring-up | monitoring-down
#                 | web-install | web-dev | web-build | web-lint | web-test
param([Parameter(Position = 0)][string]$Target = 'help')

$ErrorActionPreference = 'Stop'
$Binary = 'runmesh'
$BinDir = 'bin'

# The Go packages this repository owns, named rather than globbed — the same
# decision as the Makefile's PKG, for the same reason. web/ carries a
# node_modules tree and npm packages sometimes ship Go source of their own, so
# `./...` would put a stranger's package into this module's build graph. gofmt
# walks directories rather than package patterns, so it needs its own list.
$Pkg = @('./cmd/...', './internal/...', './migrations/...', './tests/...')
$GoSrc = @('cmd', 'internal', 'migrations', 'tests')

# The host port the local PostgreSQL is published on. A native PostgreSQL owns
# 5432 on a lot of developer machines, so this is overridable and the override
# reaches both docker compose and the connection string:
#
#   $env:RUNMESH_DB_PORT = '5433'; ./task.ps1 test-pg
if (-not $env:RUNMESH_DB_PORT) { $env:RUNMESH_DB_PORT = '5432' }
$DbPort = $env:RUNMESH_DB_PORT

# The monitoring stack's host ports, overridable the same way. Grafana's own
# default is 3000 and is deliberately not used: `next dev` in web/ takes 3000,
# and a Grafana that failed to bind is not obvious from a browser tab that
# shows a Next.js page.
if (-not $env:RUNMESH_PROMETHEUS_PORT) { $env:RUNMESH_PROMETHEUS_PORT = '9090' }
if (-not $env:RUNMESH_GRAFANA_PORT) { $env:RUNMESH_GRAFANA_PORT = '3001' }
$PromPort = $env:RUNMESH_PROMETHEUS_PORT
$GrafanaPort = $env:RUNMESH_GRAFANA_PORT

# The k6 script 'load' runs. Override it for a second scenario rather than
# adding a second target that has to be kept in step with this one:
#   $env:K6_SCRIPT = 'tests/load/soak.js'; ./task.ps1 load
if (-not $env:K6_SCRIPT) { $env:K6_SCRIPT = 'tests/load/smoke.js' }
$K6Script = $env:K6_SCRIPT

# The Go race detector needs cgo and a 64-bit C compiler. A 32-bit MinGW on
# PATH (a very common Windows setup) fails with "64-bit mode not compiled in",
# so prefer a known-good x86_64 toolchain when one is installed.
function Use-RaceToolchain {
    if ($env:RUNMESH_SKIP_CC_DETECT) { return }
    foreach ($dir in @('C:\msys64\mingw64\bin', 'C:\mingw64\bin', 'C:\tools\mingw64\bin')) {
        $gcc = Join-Path $dir 'gcc.exe'
        if (Test-Path $gcc) {
            if ((& $gcc -dumpmachine) -match 'x86_64') {
                $env:PATH = "$dir;$env:PATH"
                $env:CC = $gcc
                return
            }
        }
    }
    $onPath = Get-Command gcc -ErrorAction SilentlyContinue
    if (-not $onPath -or (& gcc -dumpmachine) -notmatch 'x86_64') {
        Write-Host 'warning: no 64-bit C compiler found; -race needs one.' -ForegroundColor Yellow
        Write-Host '         install with:  choco install mingw   (or run the suite in WSL2)' -ForegroundColor Yellow
    }
}

# k6 is a separate binary that is not a Go module and cannot be `go run`. It is
# not installed by anything in this repository, so the honest failure is to say
# which command installs it rather than to let PowerShell report
# "The term 'k6' is not recognized" and leave the reader guessing.
function Use-K6 {
    if ($env:RUNMESH_SKIP_K6_DETECT) { return }
    if (Get-Command k6 -ErrorAction SilentlyContinue) { return }
    foreach ($dir in @("$env:ProgramFiles\k6", "$env:ChocolateyInstall\bin", "$env:LOCALAPPDATA\Microsoft\WinGet\Links")) {
        if ($dir -and (Test-Path (Join-Path $dir 'k6.exe'))) {
            $env:PATH = "$dir;$env:PATH"
            return
        }
    }
    Write-Host 'warning: k6 is not on PATH; the load target needs it.' -ForegroundColor Yellow
    Write-Host '         install with:  winget install Grafana.k6   (or: choco install k6)' -ForegroundColor Yellow
}

# Node is a separate toolchain that nothing in this repository installs, so the
# honest failure is to name the command that installs it rather than to let
# PowerShell report "The term 'npm' is not recognized" and leave the reader
# guessing which of the two toolchains is missing.
function Use-Node {
    if ($env:RUNMESH_SKIP_NODE_DETECT) { return }
    if (Get-Command npm -ErrorAction SilentlyContinue) { return }
    Write-Host 'warning: npm is not on PATH; the web targets need Node.' -ForegroundColor Yellow
    Write-Host '         install with:  winget install OpenJS.NodeJS.LTS' -ForegroundColor Yellow
}

function Invoke-Step($Name, [scriptblock]$Body) {
    Write-Host "==> $Name" -ForegroundColor Cyan
    & $Body
    if ($LASTEXITCODE -ne 0) { throw "$Name failed (exit $LASTEXITCODE)" }
}

switch ($Target) {
    'build' { Invoke-Step 'build' { go build -o "$BinDir/$Binary.exe" ./cmd/server } }
    'run'   { go run ./cmd/server }
    'test'  { Invoke-Step 'test' { go test -count=1 @Pkg } }
    'race'  { Use-RaceToolchain; Invoke-Step 'race' { go test -race -count=1 @Pkg } }
    'db-up'    { Invoke-Step 'db-up' { docker compose up -d --wait postgres } }
    'db-down'  { Invoke-Step 'db-down' { docker compose stop postgres } }
    'db-reset' { Invoke-Step 'db-reset' { docker compose down -v } }
    'monitoring-up' {
        # Behind the `monitoring` compose profile, so db-up and test-pg pull
        # and wait for nothing new. RunMesh itself is not a compose service —
        # start it separately and Prometheus finds it on
        # host.docker.internal:8080.
        Invoke-Step 'monitoring-up' { docker compose --profile monitoring up -d --wait prometheus grafana }
        Write-Host "prometheus  http://127.0.0.1:$PromPort/targets" -ForegroundColor Green
        Write-Host "grafana     http://127.0.0.1:$GrafanaPort/d/runmesh-overview" -ForegroundColor Green
    }
    'monitoring-down' {
        # Stop rather than down, so the samples already collected survive and
        # "it was slow ten minutes ago" is still answerable.
        Invoke-Step 'monitoring-down' { docker compose --profile monitoring stop prometheus grafana }
    }
    'test-pg' {
        # The PostgreSQL conformance suite and the crash-recovery test SKIP when
        # this is unset, so 'test' stays useful without a database. This target
        # is the one that proves the durable store actually works.
        & $PSCommandPath db-up
        Use-RaceToolchain
        $env:RUNMESH_TEST_DATABASE_URL = "postgres://runmesh:runmesh@127.0.0.1:$DbPort/runmesh?sslmode=disable"
        Invoke-Step 'test-pg' { go test -race -count=1 @Pkg }
    }
    'test-integration' {
        # The integration suite drives the assembled server rather than a
        # package, so it is slow, it needs a database, and it is behind the
        # `integration` build tag — which is what keeps plain 'test' a
        # unit-test loop somebody will actually run between edits.
        & $PSCommandPath db-up
        $env:RUNMESH_TEST_DATABASE_URL = "postgres://runmesh:runmesh@127.0.0.1:$DbPort/runmesh?sslmode=disable"
        Invoke-Step 'test-integration' { go test -count=1 -tags=integration ./tests/integration/... }
    }
    'bench' { Invoke-Step 'bench' { go test -run '^$' -bench . -benchmem @Pkg } }
    'load' {
        # k6 drives a RunMesh that is already running; it does not start one,
        # because a load generator that owns the lifecycle of the thing it
        # measures cannot be pointed at a different one. k6 passes the whole
        # process environment through to __ENV by default, so RUNMESH_URL and
        # RUNMESH_API_KEY set in this shell reach the script with no -e
        # plumbing here.
        Use-K6
        Invoke-Step 'load' { k6 run $K6Script }
    }
    'vet'   { Invoke-Step 'vet' { go vet @Pkg } }
    'fmt'   { Invoke-Step 'fmt' { gofmt -s -w @GoSrc } }
    'tidy'  { Invoke-Step 'tidy' { go mod tidy } }
    'cover' {
        Use-RaceToolchain
        Invoke-Step 'cover' { go test -race -count=1 -covermode=atomic -coverprofile=coverage.out @Pkg }
        go tool cover -func=coverage.out | Select-Object -Last 1
        go tool cover -html=coverage.out -o coverage.html
        Write-Host 'wrote coverage.html' -ForegroundColor Green
    }
    'lint' {
        $unformatted = gofmt -l @GoSrc
        if ($unformatted) { Write-Host "not gofmt'd:" -ForegroundColor Red; $unformatted; exit 1 }
        Invoke-Step 'vet' { go vet @Pkg }
    }
    'check' { & $PSCommandPath lint; & $PSCommandPath race }
    # The dashboard has its own toolchain. These are thin on purpose: anybody
    # working on web/ for more than a minute will run npm from there directly,
    # and a wrapper trying to be more than a shortcut would just be a second
    # place for package.json's scripts to drift from. `npm ci` rather than
    # `npm install` so the install is reproducible rather than merely repeated.
    'web-install' { Use-Node; Invoke-Step 'web-install' { npm --prefix web ci } }
    'web-dev'     { Use-Node; npm --prefix web run dev }
    'web-build'   { Use-Node; Invoke-Step 'web-build' { npm --prefix web run build } }
    'web-lint'    { Use-Node; Invoke-Step 'web-lint' { npm --prefix web run lint } }
    'web-test'    { Use-Node; Invoke-Step 'web-test' { npm --prefix web test } }
    'clean' {
        Remove-Item -Recurse -Force $BinDir, coverage.out, coverage.html, web/.next, web/out -ErrorAction SilentlyContinue
        Write-Host 'cleaned' -ForegroundColor Green
    }
    default {
        Write-Host 'RunMesh tasks:'
        'build  compile the server into ./bin',
        'run    run the server from source',
        'test   run all tests',
        'race   run all tests under the race detector',
        'test-pg  the whole suite against the local PostgreSQL',
        'test-integration  the integration suite against the local PostgreSQL',
        'db-up    start the local PostgreSQL',
        'db-down  stop it, keeping its data',
        'db-reset destroy it and its data',
        'monitoring-up    start prometheus + grafana and wait for them',
        'monitoring-down  stop them, keeping their data',
        'cover  tests + HTML coverage report',
        'bench  run benchmarks',
        'load   run the k6 load script against a running server',
        'vet    go vet',
        'fmt    gofmt -s -w (Go sources only)',
        'lint   formatting check + vet',
        'check  lint + race (what CI runs)',
        'tidy   go mod tidy',
        'clean  remove build/coverage output',
        'web-install  install the dashboard deps from the lockfile',
        'web-dev      run the dashboard in development mode on :3000',
        'web-build    build the dashboard (also its type check)',
        'web-lint     lint the dashboard',
        'web-test     run the dashboard unit tests' | ForEach-Object { Write-Host "  $_" }
    }
}
