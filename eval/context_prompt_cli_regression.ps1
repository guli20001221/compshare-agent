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

function Sum-OutcomeField($records, [string]$field) {
    $sum = 0
    foreach ($rec in $records) {
        if ($rec.outcome -and $rec.outcome.PSObject.Properties.Name -contains $field -and $rec.outcome.$field) {
            $sum += [int]$rec.outcome.$field
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
    $plannerRecords = @($records | Where-Object { $null -ne $_.intent_router })
    $firstPlanner = $plannerRecords | Select-Object -First 1
    $intent = if ($firstPlanner) { [string]$firstPlanner.intent_router.intent } else { "" }
    $schemaInvalid = @($plannerRecords | Where-Object { -not $_.intent_router.schema_valid }).Count
    $escaped = Sum-Escaped $records
    $promptTokens = Sum-OutcomeField $records "prompt_tokens"
    $completionTokens = Sum-OutcomeField $records "completion_tokens"
    $totalTokens = Sum-OutcomeField $records "total_tokens"
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
    # Intent label is OBSERVED, not gated. ds-v4-flash classifies clear operation
    # commands as "unknown" ~40-60% of the time, but unknown is not a refuse path:
    # it falls through to ReAct, which still selects the correct *Workflow tool
    # (verified 10/10 on shutdown). Gating on the label made the goldens flaky on a
    # signal that does not affect execution. We gate on actual behavior (tool
    # actions); the intent mismatch is recorded for visibility only.
    $intentMismatch = $false
    if ($case.expect_intent) {
        $intentMismatch = ($intent -ne [string]$case.expect_intent)
    }
    # Positive behavior assertion: when a case names allowed_actions, at least one
    # must actually have been selected. This catches the genuinely dangerous flip
    # (a write request misrouted onto a deterministic read handler never reaches
    # ReAct, so the expected *Workflow tool is never called) while ignoring benign
    # unknown<->operation_lifecycle label jitter.
    $allowedActionMiss = $false
    if ($case.allowed_actions) {
        $hit = $false
        foreach ($allow in $case.allowed_actions) {
            if ($actions -contains [string]$allow) { $hit = $true; break }
        }
        $allowedActionMiss = (-not $hit)
    }
    $pass = ($schemaInvalid -eq 0) -and ($escaped -eq 0) -and
        ($forbiddenIntentHits.Count -eq 0) -and ($forbiddenActionHits.Count -eq 0) -and
        (-not $allowedActionMiss) -and
        ($plannerRecords.Count -gt 0)
    $result = [PSCustomObject]@{
        id = [string]$case.id
        expected_intent = [string]$case.expect_intent
        intent = $intent
        schema_invalid = $schemaInvalid
        escaped_hallucinated = $escaped
        actual_execution_paths = @($records | Where-Object { $_.actual_execution_path } | ForEach-Object { [string]$_.actual_execution_path } | Sort-Object -Unique)
        tool_actions = @($actions | Sort-Object -Unique)
        projected_tool_count = $projectedToolCount
        projection_fired = ($projectedToolCount -gt 0)
        prompt_tokens = $promptTokens
        completion_tokens = $completionTokens
        total_tokens = $totalTokens
        forbidden_intent_hits = $forbiddenIntentHits
        forbidden_action_hits = $forbiddenActionHits
        allowed_action_miss = $allowedActionMiss
        intent_mismatch_observed = $intentMismatch
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
    token_totals = [PSCustomObject]@{
        prompt_tokens = [int]((@($results | ForEach-Object { $_.prompt_tokens }) | Measure-Object -Sum).Sum)
        completion_tokens = [int]((@($results | ForEach-Object { $_.completion_tokens }) | Measure-Object -Sum).Sum)
        total_tokens = [int]((@($results | ForEach-Object { $_.total_tokens }) | Measure-Object -Sum).Sum)
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
