#Requires -Version 5.1
# Full daemon rehearsal on Windows, without a Codex account.
#
# Windows is the platform neither the author nor the maintainer can try by hand,
# so this is the only thing standing between a Win32 build that compiles and one
# that works. It exercises real process spawning, the event stream and the
# limit wait — against a mock that speaks the schema from the Codex source.

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$tmp  = Join-Path ([System.IO.Path]::GetTempPath()) ("crescent-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp -Force | Out-Null

try {
    $env:CODEX_HOME     = Join-Path $tmp 'codex'
    $env:XDG_CACHE_HOME = Join-Path $tmp 'cache'
    $workspace          = Join-Path $tmp 'workspace'
    $sessions           = Join-Path $env:CODEX_HOME 'sessions\2026\08\29'
    New-Item -ItemType Directory -Path $sessions, $workspace, (Join-Path $tmp 'bin') -Force | Out-Null

    $sid = '01a04f59-3ac3-7790-9f25-a0bd6c10ca2b'
    Set-Content -Path (Join-Path $env:CODEX_HOME 'session_index.jsonl') -Encoding utf8 `
        -Value ('{"id":"' + $sid + '","thread_name":"Konsole"}')

    $wsJson  = $workspace -replace '\\', '\\'
    $rollout = Join-Path $sessions ("rollout-a-$sid.jsonl")
    Set-Content -Path $rollout -Encoding utf8 -Value @(
        '{"type":"session_meta","cwd":"' + $wsJson + '"}',
        '{"type":"goal","payload":{"goal":{"objective":"drive Konsole to a working build","status":"active"}}}'
    )
    # Old enough that the daemon does not mistake it for a human at work.
    (Get-Item $rollout).LastWriteTime = (Get-Date).AddDays(-2)

    Push-Location $root
    $mock     = Join-Path $tmp 'bin\codex.exe'
    $crescent = Join-Path $tmp 'crescent.exe'
    & go build -o $mock ./ci/mockcodex
    & go build -o $crescent ./cmd/crescent
    if ($LASTEXITCODE -ne 0) { throw 'build failed' }

    $env:MOCK_COUNTER   = Join-Path $tmp 'counter'
    $env:CRESCENT_CODEX = $mock

    Write-Host '--- doctor ---'
    & $crescent -doctor

    Write-Host '--- goals ---'
    $list = & $crescent -list
    $list
    if ($list -notmatch 'Konsole') { throw 'chat name from the index was not picked up' }

    Write-Host '--- one supervised turn ---'
    $once = & $crescent -run-once -read-only -yes
    $once
    if ($once -notmatch 'turn\.completed') { throw 'the event stream was not parsed' }

    Write-Host '--- daemon: self-check, turns, limit, wait ---'
    Set-Content -Path $env:MOCK_COUNTER -Value '0'
    $log = Join-Path $tmp 'run.log'
    $daemon = Start-Process -FilePath $crescent -ArgumentList '-run' `
        -RedirectStandardOutput $log -RedirectStandardError (Join-Path $tmp 'run.err') `
        -PassThru -NoNewWindow

    $reached = $false
    foreach ($_ in 1..60) {
        Start-Sleep -Seconds 1
        $status = & $crescent -status 2>$null
        if ($status -match 'лимит') { $reached = $true; break }
    }
    if (-not $daemon.HasExited) { Stop-Process -Id $daemon.Id -Force }

    Write-Host '--- daemon log ---'
    if (Test-Path $log) { Get-Content $log }
    if (-not $reached) { throw 'the daemon never reached the limit wait' }

    Write-Host 'REHEARSAL PASSED'
    Pop-Location
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
