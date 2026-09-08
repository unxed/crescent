#Requires -Version 5.1
# Full daemon rehearsal on Windows, without a Codex account.
#
# Windows is the platform neither the author nor the maintainer can try by hand,
# so this is the only thing standing between a Win32 build that compiles and one
# that works. It exercises real process spawning, the event stream and the
# limit wait — against a mock that speaks the schema from the Codex source.

$ErrorActionPreference = 'Stop'

# Write-Lines puts one record per line, as JSONL requires, with LF endings and
# no byte-order mark — none of which Set-Content guarantees.
function Write-Lines([string]$Path, [string[]]$Lines) {
    [System.IO.File]::WriteAllText($Path, ($Lines -join "`n") + "`n",
        (New-Object System.Text.UTF8Encoding $false))
}

$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$tmp  = Join-Path ([System.IO.Path]::GetTempPath()) ("crescent-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp -Force | Out-Null

try {
    $env:CODEX_HOME     = Join-Path $tmp 'codex'
    $env:XDG_CACHE_HOME = Join-Path $tmp 'cache'
    $workspace          = Join-Path $tmp 'workspace'
    $sessions           = Join-Path $env:CODEX_HOME 'sessions\2026\08\29'
    New-Item -ItemType Directory -Path $sessions, $workspace, (Join-Path $tmp 'bin') -Force | Out-Null

    $sid = '01a04f59-3ac3-7790-9f25-a0bd6c10ca2b'

    # Records are built with ConvertTo-Json and written with an explicit
    # newline join. Both matter. Hand-quoted JSON meant escaping a Windows path
    # by hand, and `Set-Content -Value @(...)` does not write one array element
    # per line — it stringifies the array, joining with a space, so the two
    # records landed on one line and the file stopped being JSONL at all. The
    # index survived only because it is a single line, which is why the symptom
    # looked like "the goal is missing" rather than "the file is malformed".
    $records = @(
        @{ type = 'session_meta'; cwd = $workspace } | ConvertTo-Json -Compress
        @{ type    = 'goal'
           payload = @{ goal = @{ objective = 'drive Konsole to a working build'
                                  status    = 'active' } } } | ConvertTo-Json -Compress -Depth 6
    )
    $rollout = Join-Path $sessions ("rollout-a-$sid.jsonl")
    Write-Lines $rollout $records

    Write-Lines (Join-Path $env:CODEX_HOME 'session_index.jsonl') @(
        @{ id = $sid; thread_name = 'Konsole' } | ConvertTo-Json -Compress
    )

    # Old enough that the daemon does not mistake it for a human at work.
    (Get-Item $rollout).LastWriteTime = (Get-Date).AddDays(-2)

    Push-Location $root
    $mock     = Join-Path $tmp 'bin\codex.exe'
    $crescent = Join-Path $tmp 'crescent.exe'
    & go build -o $mock ./archive/ci/mockcodex
    & go build -o $crescent ./archive/cmd/crescent
    if ($LASTEXITCODE -ne 0) { throw 'build failed' }

    $env:MOCK_COUNTER   = Join-Path $tmp 'counter'
    $env:CRESCENT_CODEX = $mock

    Write-Host '--- doctor ---'
    & $crescent -doctor

    Write-Host '--- goals ---'
    # Joined before matching, deliberately. Against an array, -match and
    # -notmatch filter instead of testing: `if ($lines -notmatch 'x')` is true
    # whenever any single line fails to match, which is nearly always.
    $list = & $crescent -list
    $list
    if (($list -join "`n") -notmatch 'Konsole') {
        # A failure here used to say only "no goals", which could mean anything.
        Write-Host '--- фикстура, как она легла на диск ---'
        Get-Content $rollout | ForEach-Object { Write-Host "   $_" }
        & $crescent -dump
        throw 'chat name from the index was not picked up'
    }

    Write-Host '--- one supervised turn ---'
    $once = & $crescent -run-once -read-only -yes
    $once
    if (($once -join "`n") -notmatch 'turn\.completed') {
        throw 'the event stream was not parsed'
    }

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
        if (($status -join "`n") -match 'лимит') { $reached = $true; break }
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
