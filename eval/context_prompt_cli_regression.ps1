param(
    [string]$CasesPath = "",
    [string]$Tag = "context-prompt",
    [switch]$EnableProjection,
    [switch]$EnableHistoryCompaction,
    [switch]$EnableSessionFactContext,
    [switch]$SkipBuild,
    [ValidateSet("0", "1")][string]$Mutating = "1",
    [string]$ReportPath = ""
)

$ErrorActionPreference = "Continue"
$OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::InputEncoding = [System.Text.Encoding]::UTF8

function Read-TraceRecords([string]$dir) {
    $records = @()
    Get-ChildItem -Path $dir -Filter "agent-trace-*.jsonl" -File -ErrorAction SilentlyContinue | ForEach-Object {
        Get-Content -Path $_.FullName -Encoding UTF8 | Where-Object { $_.Trim() } | ForEach-Object {
            try {
                $records += ($_ | ConvertFrom-Json)
            } catch {
                Write-Host "WARN: failed to parse trace line in $($_.FullName)" -ForegroundColor Yellow
            }
        }
    }
    return $records
}

function Get-ToolActions($records) {
    $actions = @()
    foreach ($rec in $records) {
        if ($rec.tool_calls) {
            foreach ($call in $rec.tool_calls) {
                if ($call.action) { $actions += [string]$call.action }
            }
        }
    }
    return $actions
}

function Sum-Escaped($records) {
    $sum = 0
    foreach ($rec in $records) {
        if ($rec.outcome -and $rec.outcome.escaped_hallucinated_count) {
            $sum += [int]$rec.outcome.escaped_hallucinated_count
        }
    }
    return $sum
}

function Invoke-AgentCase($agentExe, $config, $case, $caseDir) {
    New-Item -ItemType Directory -Force -Path $caseDir | Out-Null
    $env:COMPSHARE_TRACE_DIR = $caseDir
    $stdinItems = @()
    if ($case.stdin) {
        foreach ($item in $case.stdin) { $stdinItems += [string]$item }
    } else {
        $stdinItems += [string]$case.question
        $stdinItems += "exit"
    }
    $stdinText = ($stdinItems -join "`n") + "`n"
    $inputPath = Join-Path $caseDir "stdin.txt"
    $transcriptPath = Join-Path $caseDir "transcript.txt"
    [IO.File]::WriteAllText($inputPath, $stdinText, (New-Object Text.UTF8Encoding $false))
    $output = (Get-Content $inputPath -Raw -Encoding UTF8) | & $agentExe cli -c $config 2>&1 | Out-String
    $output | Set-Content -Path $transcriptPath -Encoding UTF8
}

$repoRoot = Split-Path -Parent $PSScriptRoot
if (-not $CasesPath) { $CasesPath = Join-Path $PSScriptRoot "context_prompt_cli_regression_cases.json" }
$agentExe = Join-Path $repoRoot "agent.exe"
$config = Join-Path $repoRoot "deploy\conf\agent.yaml"
$smokeEnv = Join-Path $PSScriptRoot ".smoke_env.ps1"

if (Test-Path $smokeEnv) {
    . $smokeEnv
}
if (-not $env:LLM_API_KEY) {
    Write-Host "LLM_API_KEY is not set; cannot run real CLI regression." -ForegroundColor Red
    exit 1
}
if (-not $SkipBuild) {
    Push-Location $repoRoot
    try {
        go build -o $agentExe ./cmd
        if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
    } finally {
        Pop-Location
    }
} elseif (-not (Test-Path $agentExe)) {
    Push-Location $repoRoot
    try {
        go build -o $agentExe ./cmd
        if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }
    } finally {
        Pop-Location
    }
}

$env:COMPSHARE_TRACE_ENABLED = "1"
$env:MYSQL_DSN = ""
$env:COMPSHARE_ENABLE_MUTATING_TOOLS = $Mutating
$env:USE_REACT_RESULT_PROJECTION = $(if ($EnableProjection) { "1" } else { "" })
$env:USE_REACT_HISTORY_COMPACTION = $(if ($EnableHistoryCompaction) { "1" } else { "" })
$env:USE_SESSION_FACT_CONTEXT = $(if ($EnableSessionFactContext) { "1" } else { "" })
if (-not $env:COMPSHARE_PROJECT_ID) {
    $env:COMPSHARE_PROJECT_ID = "org-cwy2qk"
}

$runId = Get-Date -Format "yyyyMMdd-HHmmss"
$baseDir = Join-Path $env:TEMP "compshare-$Tag-cli-$runId"
New-Item -ItemType Directory -Force -Path $baseDir | Out-Null
if (-not $ReportPath) {
    $ReportPath = Join-Path $baseDir "summary.json"
}

$cases = [IO.File]::ReadAllText($CasesPath, [Text.Encoding]::UTF8) | ConvertFrom-Json
$results = @()

foreach ($case in $cases) {
    $caseDir = Join-Path $baseDir ([string]$case.id)
    Write-Host ">>> [$($case.id)] expected=$($case.expect_intent)" -ForegroundColor Cyan
    Invoke-AgentCase $agentExe $config $case $caseDir
    $records = @(Read-TraceRecords $caseDir)
    $plannerRecords = @($records | Where-Object { $null -ne $_.planner })
    $firstPlanner = $plannerRecords | Select-Object -First 1
    $intent = if ($firstPlanner) { [string]$firstPlanner.planner.intent } else { "" }
    $schemaInvalid = @($plannerRecords | Where-Object { -not $_.planner.schema_valid }).Count
    $escaped = Sum-Escaped $records
    $actions = @(Get-ToolActions $records)
    # Count tool results that ReAct projection actually shrank (trace field
    # tool_calls[].projected, emitted only when projection fired). When run with
    # -EnableProjection, a read case whose eligible tool returns 0 here means
    # projection no-op'd on the real API field names — the signal we want surfaced.
    $projectedToolCount = 0
    foreach ($rec in $records) {
        foreach ($tc in @($rec.tool_calls)) {
            if ($tc.projected) { $projectedToolCount++ }
        }
    }
    $forbiddenIntentHits = @()
    if ($case.forbid_intents) {
        foreach ($forbid in $case.forbid_intents) {
            if ($intent -eq [string]$forbid) { $forbiddenIntentHits += [string]$forbid }
        }
    }
    $forbiddenActionHits = @()
    if ($case.forbid_actions) {
        foreach ($forbid in $case.forbid_actions) {
            if ($actions -contains [string]$forbid) { $forbiddenActionHits += [string]$forbid }
        }
    }
    $intentOK = $true
    if ($case.expect_intent) {
        $intentOK = ($intent -eq [string]$case.expect_intent)
    }
    $pass = $intentOK -and ($schemaInvalid -eq 0) -and ($escaped -eq 0) -and
        ($forbiddenIntentHits.Count -eq 0) -and ($forbiddenActionHits.Count -eq 0) -and
        ($plannerRecords.Count -gt 0)
    $result = [PSCustomObject]@{
        id = [string]$case.id
        expected_intent = [string]$case.expect_intent
        intent = $intent
        schema_invalid = $schemaInvalid
        escaped_hallucinated = $escaped
        actual_runtime_forms = @($records | Where-Object { $_.actual_runtime_form } | ForEach-Object { [string]$_.actual_runtime_form } | Sort-Object -Unique)
        tool_actions = @($actions | Sort-Object -Unique)
        projected_tool_count = $projectedToolCount
        projection_fired = ($projectedToolCount -gt 0)
        forbidden_intent_hits = $forbiddenIntentHits
        forbidden_action_hits = $forbiddenActionHits
        mutating_case = [bool]$case.mutating_case
        trace_dir = $caseDir
        pass = $pass
    }
    $results += $result
    $color = if ($pass) { "Green" } else { "Red" }
    Write-Host "    intent=$intent schema_invalid=$schemaInvalid escaped=$escaped pass=$pass" -ForegroundColor $color
}

$summary = [PSCustomObject]@{
    tag = $Tag
    started_at = $runId
    trace_dir = $baseDir
    flags = [PSCustomObject]@{
        use_react_result_projection = [bool]$EnableProjection
        use_react_history_compaction = [bool]$EnableHistoryCompaction
        use_session_fact_context = [bool]$EnableSessionFactContext
        compshare_enable_mutating_tools = $Mutating
    }
    projection = [PSCustomObject]@{
        enabled = [bool]$EnableProjection
        total_projected_tool_calls = [int]((@($results | ForEach-Object { $_.projected_tool_count }) | Measure-Object -Sum).Sum)
        cases_with_projection = @($results | Where-Object { $_.projection_fired }).Count
    }
    pass = (@($results | Where-Object { -not $_.pass }).Count -eq 0)
    results = $results
}

$summary | ConvertTo-Json -Depth 12 | Set-Content -Path $ReportPath -Encoding UTF8
$summary | ConvertTo-Json -Depth 12
if (-not $summary.pass) {
    exit 1
}
