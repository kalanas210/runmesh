#!/usr/bin/env pwsh
# RunMesh task runner for Windows (GNU make equivalent).
#   ./task.ps1 build | run | test | test-pg | race | cover | bench | vet | fmt
#                 | lint | check | tidy | clean | db-up | db-down | db-reset
param([Parameter(Position = 0)][string]$Target = 'help')

$ErrorActionPreference = 'Stop'
$Binary = 'runmesh'
$BinDir = 'bin'

# The host port the local PostgreSQL is published on. A native PostgreSQL owns
# 5432 on a lot of developer machines, so this is overridable and the override
# reaches both docker compose and the connection string:
#
#   $env:RUNMESH_DB_PORT = '5433'; ./task.ps1 test-pg
if (-not $env:RUNMESH_DB_PORT) { $env:RUNMESH_DB_PORT = '5432' }
$DbPort = $env:RUNMESH_DB_PORT

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

function Invoke-Step($Name, [scriptblock]$Body) {
    Write-Host "==> $Name" -ForegroundColor Cyan
    & $Body
    if ($LASTEXITCODE -ne 0) { throw "$Name failed (exit $LASTEXITCODE)" }
}

switch ($Target) {
    'build' { Invoke-Step 'build' { go build -o "$BinDir/$Binary.exe" ./cmd/server } }
    'run'   { go run ./cmd/server }
    'test'  { Invoke-Step 'test' { go test -count=1 ./... } }
    'race'  { Use-RaceToolchain; Invoke-Step 'race' { go test -race -count=1 ./... } }
    'db-up'    { Invoke-Step 'db-up' { docker compose up -d --wait postgres } }
    'db-down'  { Invoke-Step 'db-down' { docker compose stop postgres } }
    'db-reset' { Invoke-Step 'db-reset' { docker compose down -v } }
    'test-pg' {
        # The PostgreSQL conformance suite and the crash-recovery test SKIP when
        # this is unset, so 'test' stays useful without a database. This target
        # is the one that proves the durable store actually works.
        & $PSCommandPath db-up
        Use-RaceToolchain
        $env:RUNMESH_TEST_DATABASE_URL = "postgres://runmesh:runmesh@127.0.0.1:$DbPort/runmesh?sslmode=disable"
        Invoke-Step 'test-pg' { go test -race -count=1 ./... }
    }
    'bench' { Invoke-Step 'bench' { go test -run '^$' -bench . -benchmem ./... } }
    'vet'   { Invoke-Step 'vet' { go vet ./... } }
    'fmt'   { Invoke-Step 'fmt' { gofmt -s -w . } }
    'tidy'  { Invoke-Step 'tidy' { go mod tidy } }
    'cover' {
        Use-RaceToolchain
        Invoke-Step 'cover' { go test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./... }
        go tool cover -func=coverage.out | Select-Object -Last 1
        go tool cover -html=coverage.out -o coverage.html
        Write-Host 'wrote coverage.html' -ForegroundColor Green
    }
    'lint' {
        $unformatted = gofmt -l .
        if ($unformatted) { Write-Host "not gofmt'd:" -ForegroundColor Red; $unformatted; exit 1 }
        Invoke-Step 'vet' { go vet ./... }
    }
    'check' { & $PSCommandPath lint; & $PSCommandPath race }
    'clean' {
        Remove-Item -Recurse -Force $BinDir, coverage.out, coverage.html -ErrorAction SilentlyContinue
        Write-Host 'cleaned' -ForegroundColor Green
    }
    default {
        Write-Host 'RunMesh tasks:'
        'build  compile the server into ./bin',
        'run    run the server from source',
        'test   run all tests',
        'race   run all tests under the race detector',
        'test-pg  the whole suite against the local PostgreSQL',
        'db-up    start the local PostgreSQL',
        'db-down  stop it, keeping its data',
        'db-reset destroy it and its data',
        'cover  tests + HTML coverage report',
        'bench  run benchmarks',
        'vet    go vet',
        'fmt    gofmt -s -w .',
        'lint   formatting check + vet',
        'check  lint + race (what CI runs)',
        'tidy   go mod tidy',
        'clean  remove build/coverage output' | ForEach-Object { Write-Host "  $_" }
    }
}
