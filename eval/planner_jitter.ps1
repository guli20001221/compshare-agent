# Planner jitter check - measure SAME-question classification variance on the
# current planner model (deepseek-v4-flash, from agent.yaml) to inform roadmap
# open-decision #1 (planner pro-vs-flash). For each question it runs N CLI turns
# and reads planner.intent + planner.schema_valid from the per-question trace,
# then reports distinct-intent count and schema_valid flapping per question.
#
# This is a RELIABILITY signal, not an accuracy one: it measures whether the
# planner gives the SAME answer to the SAME input, not whether that answer is
# correct. A control question that stays distinct=1 validates the harness; a
# borderline question with distinct>1 or schema_invalid>0 is jitter.
#
# Needs only LLM_API_KEY (the planner is a pure LLM Chat call - no tools, no
# MySQL). STS/CompShare creds are OPTIONAL: they are sourced from the gitignored
# start-server.ps1 so instance-dependent questions get production-representative
# registry context, but classification still runs if STS is absent/broken
# (engine Init is best-effort; the planner runs before any CompShare tool call).
# This script never prints or hard-codes secrets.
#
# MUTATING FLAG: the DEPLOYED console runs with COMPSHARE_ENABLE_MUTATING_TOOLS=1
# (write on); the *code* default is read-only (engine.go:392). The flag does NOT
# affect planner classification — the planner prompt (internal/intent/planner.go
# base block) is built unconditionally; the flag only feeds the ReAct system
# prompt (engine.go:594), tool-registry visibility (registry.go:801), and the
# safe executor, all downstream of planner.intent. So jitter numbers are
# identical on or off. -Mutating defaults to "0" purely for instance-safety
# (zero chance of touching a real instance across N runs); pass -Mutating 1 to
# mirror prod exactly. Either way the only mutating-intent question is zero-target
# (cannot resolve a target) and any confirm prompt is declined by the trailing
# "quit" on stdin.
#
# LIMITATION (documented, not silently skipped): this harness is SINGLE-TURN.
# The PriorText-avalanche jitter mode (memory priortext-avalanche-invalidates-
# planner: input_tok 5k->11k across turns -> schema_valid=false) needs a
# multi-turn driver and is NOT covered here. Treat a clean result as "no
# single-turn jitter", not "no jitter".
#
# Usage:  pwsh -File eval\planner_jitter.ps1 [-Runs 8] [-Mutating 0|1]

param(
    [int]$Runs = 8,
    [ValidateSet("0", "1")][string]$Mutating = "0",
    [string]$Model = ""
)

$ErrorActionPreference = "Continue"
[Console]::OutputEncoding = [Text.Encoding]::UTF8
# CJK questions are piped to agent.exe stdin via `| & $agentExe`. In Windows
# PowerShell 5.1 the encoding for that pipe is $OutputEncoding (NOT
# [Console]::OutputEncoding), which DEFAULTS TO ASCII — silently turning every
# Chinese question into "?" (0x3F) before the planner sees it. Without this the
# planner classifies garbled "?????" as unknown/low-confidence and every result
# is invalid (looks like model degradation; it is not). Profile-independent so
# -NoProfile runs are correct too. See InputEncoding for the agent's stdout.
$OutputEncoding = [Text.Encoding]::UTF8
[Console]::InputEncoding = [Text.Encoding]::UTF8

# Source ONLY the $env: lines from start-server.ps1 (skip its build/run lines).
$startServer = "F:\compshare-agent\start-server.ps1"
if (Test-Path $startServer) {
    $envLines = Get-Content $startServer | Where-Object { $_ -match '^\$env:' }
    foreach ($line in $envLines) { Invoke-Expression $line }
} else {
    Write-Host "start-server.ps1 not found; set `$env:LLM_API_KEY manually first." -ForegroundColor Yellow
}

if (-not $env:LLM_API_KEY) {
    Write-Host "LLM_API_KEY not set - the planner cannot run. Aborting." -ForegroundColor Red
    exit 1
}

# No MySQL (CLI path), trace on. Leave USE_INTENT_PLANNER_FOR UNSET so the
# engine planner + trace observer auto-wire on the default 8-intent cutover set.
# Mutating is caller-controlled (-Mutating); orthogonal to planner.intent.
$env:COMPSHARE_ENABLE_MUTATING_TOOLS = $Mutating
$env:MYSQL_DSN = ""
$env:COMPSHARE_TRACE_ENABLED = "1"

$agentExe = "F:\compshare-agent\agent.exe"
$config = "F:\compshare-agent\deploy\conf\agent.yaml"
$baseDir = "F:\compshare-agent\eval\traces_planner_jitter"
$questionsPath = "F:\compshare-agent\eval\planner_jitter_questions.json"

# Oracle mode: -Model swaps the answerer+planner model via a temp config
# (agent.yaml's "deepseek-v4-flash" literal is unique — verified) and writes
# to a model-specific trace dir so it never clobbers the baseline (flash) run.
# The planner shares cfg.Agent.LLM.Model, so this puts the PLANNER on $Model,
# which is exactly the oracle comparison for decision #1.
if ($Model) {
    $baseDir = "${baseDir}_$($Model -replace '[^\w.-]', '_')"
    New-Item -ItemType Directory -Force -Path $baseDir | Out-Null
    $tmpConfig = Join-Path $baseDir "agent.oracle.yaml"
    $orig = [IO.File]::ReadAllText($config, [Text.Encoding]::UTF8)
    if ($orig -notmatch 'deepseek-v4-flash') {
        Write-Host "WARN: 'deepseek-v4-flash' not found in config; override may be incomplete." -ForegroundColor Yellow
    }
    [IO.File]::WriteAllText($tmpConfig, ($orig -replace 'deepseek-v4-flash', $Model), [Text.Encoding]::UTF8)
    $config = $tmpConfig
    Write-Host "Oracle mode: model -> $Model  (config: $tmpConfig)" -ForegroundColor Magenta
}

if (-not (Test-Path $agentExe)) {
    Write-Host "agent.exe not found - build first: go build -o agent.exe ./cmd" -ForegroundColor Red
    exit 1
}

New-Item -ItemType Directory -Force -Path $baseDir | Out-Null
$summary = Join-Path $baseDir "summary.txt"

$questionsJson = [IO.File]::ReadAllText($questionsPath, [Text.Encoding]::UTF8)
$questions = $questionsJson | ConvertFrom-Json

$report = @()

foreach ($q in $questions) {
    # Fresh per-question trace dir so the date-file holds ONLY this question's
    # N turns (the trace file appends; mixing questions would corrupt counts).
    $qDir = Join-Path $baseDir $q.qid
    if (Test-Path $qDir) { Remove-Item $qDir -Recurse -Force }
    New-Item -ItemType Directory -Force -Path $qDir | Out-Null
    $env:COMPSHARE_TRACE_DIR = $qDir

    Write-Host ""
    Write-Host ">>> [$($q.qid)] kind=$($q.kind) expect~$($q.expect_intent)  (x$Runs)" -ForegroundColor Cyan
    Write-Host "    Q: $($q.question)" -ForegroundColor Gray
    Write-Host -NoNewline "    "

    for ($i = 1; $i -le $Runs; $i++) {
        $inputText = "$($q.question)`nquit`n"
        $tmpIn = New-TemporaryFile
        # UTF-8 WITHOUT BOM — [Text.Encoding]::UTF8 emits a BOM that Get-Content
        # -Raw leaks into stdin as a leading ﻿ on the question, polluting
        # the planner input. New-Object Text.UTF8Encoding $false = no BOM.
        [IO.File]::WriteAllText($tmpIn.FullName, $inputText, (New-Object Text.UTF8Encoding $false))
        $null = (Get-Content $tmpIn.FullName -Raw -Encoding utf8) | & $agentExe cli -c $config 2>&1
        Remove-Item $tmpIn.FullName -Force
        Write-Host -NoNewline "."
    }
    Write-Host ""

    # Collect planner.intent + planner.schema_valid from every trace line.
    $intents = @()
    $schemaInvalid = 0
    Get-ChildItem -Path $qDir -Filter "agent-trace-*.jsonl" -ErrorAction SilentlyContinue | ForEach-Object {
        Get-Content $_.FullName | Where-Object { $_.Trim() } | ForEach-Object {
            try { $rec = $_ | ConvertFrom-Json } catch { return }
            if ($null -ne $rec.planner) {
                $intents += [string]$rec.planner.intent
                if (-not $rec.planner.schema_valid) { $schemaInvalid++ }
            }
        }
    }

    $grouped = $intents | Group-Object | Sort-Object Count -Descending
    $distinctCount = ($intents | Sort-Object -Unique).Count
    $spread = ($grouped | ForEach-Object { "$($_.Name)=$($_.Count)" }) -join ", "
    $stable = ($distinctCount -le 1) -and ($schemaInvalid -eq 0) -and ($intents.Count -gt 0)

    $report += [PSCustomObject]@{
        QID           = $q.qid
        Kind          = $q.kind
        N             = $intents.Count
        Distinct      = $distinctCount
        Spread        = $spread
        SchemaInvalid = $schemaInvalid
        Stable        = $stable
    }

    $tag = if ($stable) { "Green" } elseif ($intents.Count -eq 0) { "Yellow" } else { "Red" }
    Write-Host "    distinct=$distinctCount [$spread] schema_invalid=$schemaInvalid n=$($intents.Count)" -ForegroundColor $tag
}

"Planner jitter check - $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')  (Runs=$Runs per question)" | Set-Content $summary -Encoding utf8
"Model: from deploy/conf/agent.yaml (expected deepseek-v4-flash). Single-turn only." | Add-Content $summary -Encoding utf8
("=" * 72) | Add-Content $summary -Encoding utf8
($report | Format-Table -AutoSize | Out-String) | Add-Content $summary -Encoding utf8
$jittery = ($report | Where-Object { -not $_.Stable }).Count
"JITTERY questions: $jittery / $($report.Count)  (controls should be 0)" | Add-Content $summary -Encoding utf8

Write-Host ""
Write-Host "=== Planner Jitter Summary ===" -ForegroundColor Yellow
$report | Format-Table -AutoSize
Write-Host "JITTERY: $jittery / $($report.Count)  (controls MUST be stable, else harness is suspect)" -ForegroundColor $(if ($jittery -eq 0) { "Green" } else { "Red" })
Write-Host "Summary: $summary" -ForegroundColor Gray
