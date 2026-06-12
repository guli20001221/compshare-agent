param(
    [string]$AgentExe = "",
    [string]$Config = "",
    [string]$QuestionsPath = "",
    [string]$TraceDir = "",
    [string]$OutFixture = "",
    [string]$OutLabels = "",
    [int]$MaxCases = 8,
    [ValidateSet("0", "1")][string]$Mutating = "0"
)

$ErrorActionPreference = "Stop"
$OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::InputEncoding = [System.Text.Encoding]::UTF8

function Get-CaseField($obj, [string[]]$names) {
    foreach ($name in $names) {
        if ($obj.PSObject.Properties.Name -contains $name) {
            return [string]$obj.$name
        }
    }
    return ""
}

function Convert-ToGateRecord($rec) {
    $out = [ordered]@{
        schema_version = [string]$rec.schema_version
        trace_id = [string]$rec.trace_id
        turn_id = [string]$rec.turn_id
        turn_index = [int]$rec.turn_index
        timestamp = [string]$rec.timestamp
    }
    if ($rec.actual_execution_path) {
        $out.actual_execution_path = [string]$rec.actual_execution_path
    }
    if ($rec.intent_router) {
        $out.intent_router = [ordered]@{
            enabled = [bool]$rec.intent_router.enabled
            model = [string]$rec.intent_router.model
            schema_valid = [bool]$rec.intent_router.schema_valid
            intent = [string]$rec.intent_router.intent
            planned_execution_path = [string]$rec.intent_router.planned_execution_path
            confidence = [double]$rec.intent_router.confidence
            route_status = [string]$rec.intent_router.route_status
        }
    }
    if ($rec.tool_calls) {
        $out.tool_calls = @($rec.tool_calls | ForEach-Object {
            [ordered]@{
                action = [string]$_.action
                source = [string]$_.source
                status = [string]$_.status
            }
        })
    }
    if ($rec.retrieval) {
        $out.retrieval = [ordered]@{
            enabled = [bool]$rec.retrieval.enabled
            hits = [int]$rec.retrieval.hits
        }
    }
    if ($rec.outcome) {
        $out.outcome = [ordered]@{
            escaped_hallucinated_count = [int]$rec.outcome.escaped_hallucinated_count
        }
    }
    return [PSCustomObject]$out
}

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$repoRoot = Split-Path -Parent (Split-Path -Parent $scriptDir)

if (-not $AgentExe) { $AgentExe = Join-Path $repoRoot "agent.exe" }
if (-not $Config) { $Config = Join-Path $repoRoot "deploy\conf\agent.yaml" }
if (-not $QuestionsPath) { $QuestionsPath = Join-Path $repoRoot "eval\diagnosis_routing_jitter_questions.json" }
if (-not $OutFixture) { $OutFixture = Join-Path $scriptDir "fixtures\context_prompt_baseline.candidate.jsonl" }
if (-not $OutLabels) { $OutLabels = Join-Path $scriptDir "fixtures\context_prompt_baseline.candidate.labels.json" }
if (-not $TraceDir) {
    $runId = Get-Date -Format "yyyyMMdd-HHmmss"
    $TraceDir = Join-Path $env:TEMP "compshare-trace-gate-capture-$runId"
}

$smokeEnv = Join-Path $repoRoot "eval\.smoke_env.ps1"
if (Test-Path $smokeEnv) {
    . $smokeEnv
}
if (-not $env:LLM_API_KEY) {
    Write-Host "LLM_API_KEY is not set; cannot capture real CLI traces." -ForegroundColor Red
    exit 1
}
if (-not (Test-Path $AgentExe)) {
    Write-Host "agent.exe not found; building $AgentExe" -ForegroundColor Yellow
    Push-Location $repoRoot
    try {
        go build -o $AgentExe ./cmd
    } finally {
        Pop-Location
    }
}

New-Item -ItemType Directory -Force -Path $TraceDir | Out-Null
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $OutFixture) | Out-Null
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $OutLabels) | Out-Null

$env:COMPSHARE_TRACE_ENABLED = "1"
$env:COMPSHARE_TRACE_DIR = $TraceDir
$env:MYSQL_DSN = ""
$env:COMPSHARE_ENABLE_MUTATING_TOOLS = $Mutating

# Run from the repo root so the agent can resolve its relative deploy/kb corpus
# and deploy/conf paths. The script may be launched from any cwd (e.g. a
# background shell); without this the agent fails to load and writes no traces.
Set-Location -LiteralPath $repoRoot

$questions = [IO.File]::ReadAllText($QuestionsPath, [Text.Encoding]::UTF8) | ConvertFrom-Json
$selected = @($questions | Select-Object -First $MaxCases)
if ($selected.Count -eq 0) {
    Write-Host "No questions found in $QuestionsPath" -ForegroundColor Red
    exit 1
}

$expectedIntents = New-Object System.Collections.Generic.List[string]
# Each case is a separate single-turn CLI process, so every trace record carries
# the same agent turn_id ("turn-1"). The gate joins records<->labels by turn_id,
# so we must stamp a unique key per case; the question id is unique and readable.
$caseIds = New-Object System.Collections.Generic.List[string]
foreach ($q in $selected) {
    $qid = Get-CaseField $q @("id", "qid")
    if (-not $qid) { $qid = "case" }
    $question = Get-CaseField $q @("question", "input", "prompt")
    if (-not $question) {
        Write-Host "Skipping $qid because it has no question field." -ForegroundColor Yellow
        continue
    }
    $expectedIntents.Add((Get-CaseField $q @("expect", "expect_intent")))
    $caseIds.Add([string]$qid)
    Write-Host ">>> [$qid] $question" -ForegroundColor Cyan
    $stdin = "$question`nexit`n"
    $tmpIn = New-TemporaryFile
    [IO.File]::WriteAllText($tmpIn.FullName, $stdin, (New-Object Text.UTF8Encoding $false))
    try {
        # The agent writes benign warnings (e.g. the anonymous rate-limiter
        # notice) to stderr. Under the script-level $ErrorActionPreference="Stop"
        # PowerShell 5.1 turns native stderr into a terminating NativeCommandError
        # and aborts the capture. Drop to Continue for just this native call and
        # discard stderr; the trace is read from JSONL files, not stdout.
        $prevEAP = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        try {
            $null = (Get-Content $tmpIn.FullName -Raw -Encoding UTF8) | & $AgentExe cli -c $Config 2>$null
        } finally {
            $ErrorActionPreference = $prevEAP
        }
    } finally {
        Remove-Item -LiteralPath $tmpIn.FullName -Force
    }
}

$gateLines = New-Object System.Collections.Generic.List[string]
$labelCases = @()
$recordIndex = 0

Get-ChildItem -Path $TraceDir -Filter "agent-trace-*.jsonl" -File | ForEach-Object {
    Get-Content -Path $_.FullName -Encoding UTF8 | Where-Object { $_.Trim() } | ForEach-Object {
        try {
            $rec = $_ | ConvertFrom-Json
        } catch {
            return
        }
        if ($null -eq $rec.intent_router) { return }
        $caseId = if ($recordIndex -lt $caseIds.Count) { $caseIds[$recordIndex] } else { "case-$recordIndex" }
        $expect = if ($recordIndex -lt $expectedIntents.Count) { $expectedIntents[$recordIndex] } else { "" }
        $recordIndex++

        $gate = Convert-ToGateRecord $rec
        $gate.turn_id = $caseId
        $gateLines.Add(($gate | ConvertTo-Json -Depth 12 -Compress))
        if ($expect) {
            $labelCases += [PSCustomObject]@{
                turn_id = $caseId
                expected_intent = $expect
                forbidden_intent = $(if ($expect -eq "diagnosis") { "knowledge_qa" } else { "" })
                curated_schema_valid = [bool]$rec.intent_router.schema_valid
                note = "captured by trace_gate/capture_baseline.ps1"
            }
        }
    }
}

[IO.File]::WriteAllLines($OutFixture, $gateLines, (New-Object Text.UTF8Encoding $false))
$schemaValid = @($gateLines | ForEach-Object {
    $line = $_ | ConvertFrom-Json
    if ($line.intent_router.schema_valid) { 1 } else { 0 }
})
$schemaMin = if ($schemaValid.Count -gt 0) {
    [Math]::Max(0.0, (($schemaValid | Measure-Object -Sum).Sum / $schemaValid.Count) - 0.02)
} else {
    1.0
}
# Anchor the runtime-form mismatch ceiling to the measured baseline (+slack),
# same as schema_valid_rate_min. Some intents legitimately plan one form but
# execute another (e.g. pricing_query plans routing yet runs an agent step), so
# a hardcoded 0.0 would make the gate fail on any real corpus. The gate then
# flags an *increase* in mismatches from this baseline, not the baseline itself.
$formCompared = 0
$formMismatch = 0
foreach ($gl in $gateLines) {
    $line = $gl | ConvertFrom-Json
    $pf = [string]$line.intent_router.planned_execution_path
    $af = [string]$line.actual_execution_path
    if ($pf -and $af) {
        $formCompared++
        if ($pf -ne $af) { $formMismatch++ }
    }
}
$formMax = if ($formCompared -gt 0) { [Math]::Round(($formMismatch / $formCompared) + 0.02, 3) } else { 0.0 }
$labels = [PSCustomObject]@{
    thresholds = [PSCustomObject]@{
        execution_path_mismatch_rate_max = $formMax
        schema_valid_rate_min = [Math]::Round($schemaMin, 3)
    }
    cases = $labelCases
}
[IO.File]::WriteAllText($OutLabels, ($labels | ConvertTo-Json -Depth 12), (New-Object Text.UTF8Encoding $false))

Write-Host "Candidate fixture: $OutFixture" -ForegroundColor Green
Write-Host "Candidate labels:  $OutLabels" -ForegroundColor Green
Write-Host "Trace dir:          $TraceDir" -ForegroundColor Gray
Write-Host "Before committing, manually review the candidate files and grep for live tokens/secrets." -ForegroundColor Yellow
