# Diagnosis-vs-terminal-RAG routing jitter check.
# Runs each question N times through the real CLI and summarizes planner intent
# distribution. Uses live LLM planner and local smoke credentials, but writes no
# secrets and does not perform mutating operations.
param(
    [int]$Runs = 5,
    [string]$Tag = "run",
    [string]$QuestionsPath = "",
    [string]$ReportPath = ""
)

$ErrorActionPreference = "Continue"
$OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::InputEncoding = [System.Text.Encoding]::UTF8

$repoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
if (-not $QuestionsPath) {
    $QuestionsPath = Join-Path $repoRoot "eval\diagnosis_routing_jitter_questions.json"
}
$agentExe = Join-Path $repoRoot "agent.exe"
$config = Join-Path $repoRoot "deploy\conf\agent.yaml"
$smokeEnv = Join-Path $repoRoot "eval\.smoke_env.ps1"

if (Test-Path $smokeEnv) {
    . $smokeEnv
}
if (-not $env:LLM_API_KEY) {
    Write-Host "LLM_API_KEY is not set; cannot run planner jitter." -ForegroundColor Red
    exit 1
}
if (-not (Test-Path $agentExe)) {
    Write-Host "agent.exe not found; run: go build -o agent.exe ./cmd" -ForegroundColor Red
    exit 1
}

$env:COMPSHARE_PROJECT_ID = "org-cwy2qk"
$env:COMPSHARE_ENABLE_MUTATING_TOOLS = ""
$env:USE_INTENT_PLANNER_FOR = ""
$env:USE_SKILL_EXECUTOR = ""
$env:MYSQL_DSN = ""
$env:COMPSHARE_TRACE_ENABLED = "1"

$runId = Get-Date -Format "yyyyMMdd-HHmmss"
$baseDir = Join-Path $env:TEMP "compshare-diagnosis-routing-$Tag-$runId"
New-Item -ItemType Directory -Force -Path $baseDir | Out-Null

$questions = [IO.File]::ReadAllText($QuestionsPath, [Text.Encoding]::UTF8) | ConvertFrom-Json
$results = @()

foreach ($q in $questions) {
    $qDir = Join-Path $baseDir $q.id
    New-Item -ItemType Directory -Force -Path $qDir | Out-Null
    $env:COMPSHARE_TRACE_DIR = $qDir

    Write-Host ""
    Write-Host ">>> [$($q.id)] expect=$($q.expect) x$Runs" -ForegroundColor Cyan
    Write-Host "    $($q.question)" -ForegroundColor Gray

    for ($i = 1; $i -le $Runs; $i++) {
        $inputPath = Join-Path $qDir ("stdin-" + $i + ".txt")
        [IO.File]::WriteAllText($inputPath, "$($q.question)`nexit`n", (New-Object Text.UTF8Encoding $false))
        $output = (Get-Content $inputPath -Raw -Encoding UTF8) | & $agentExe cli -c $config 2>&1 | Out-String
        $output | Set-Content -Path (Join-Path $qDir ("transcript-" + $i + ".txt")) -Encoding UTF8
        Write-Host -NoNewline "."
    }
    Write-Host ""

    $intents = @()
    $plannedForms = @()
    $cutovers = @()
    $schemaInvalid = 0
    Get-ChildItem -Path $qDir -Filter "agent-trace-*.jsonl" -ErrorAction SilentlyContinue | ForEach-Object {
        Get-Content $_.FullName -Encoding UTF8 | Where-Object { $_.Trim() } | ForEach-Object {
            try { $rec = $_ | ConvertFrom-Json } catch { return }
            if ($null -ne $rec.intent_router) {
                $intents += [string]$rec.intent_router.intent
                $plannedForms += [string]$rec.intent_router.planned_runtime_form
                $cutovers += [string]$rec.intent_router.route_status
                if (-not $rec.intent_router.schema_valid) { $schemaInvalid++ }
            }
        }
    }

    $diagnosis = @($intents | Where-Object { $_ -eq "diagnosis" }).Count
    $knowledge = @($intents | Where-Object { $_ -eq "knowledge_qa" }).Count
    $byIntent = @{}
    foreach ($g in ($intents | Group-Object)) { $byIntent[$g.Name] = $g.Count }
    $byForm = @{}
    foreach ($g in ($plannedForms | Group-Object)) { $byForm[$g.Name] = $g.Count }
    $byCutover = @{}
    foreach ($g in ($cutovers | Group-Object)) { $byCutover[$g.Name] = $g.Count }

    $pass = $false
    if ($q.expect -eq "diagnosis") {
        $pass = $diagnosis -ge [Math]::Min(4, $Runs)
    } elseif ($q.expect -eq "knowledge_qa") {
        $pass = $knowledge -eq $Runs
    }

    $results += [PSCustomObject]@{
        id = $q.id
        question = $q.question
        expect = $q.expect
        runs = $Runs
        diagnosis = $diagnosis
        knowledge_qa = $knowledge
        schema_invalid = $schemaInvalid
        intents = $byIntent
        planned_forms = $byForm
        cutovers = $byCutover
        pass = $pass
    }
}

$summary = [PSCustomObject]@{
    tag = $Tag
    runs_per_question = $Runs
    trace_dir = $baseDir
    pass = (@($results | Where-Object { -not $_.pass }).Count -eq 0)
    results = $results
}

$json = $summary | ConvertTo-Json -Depth 12
if ($ReportPath) {
    $json | Set-Content -Path $ReportPath -Encoding UTF8
}
$json
