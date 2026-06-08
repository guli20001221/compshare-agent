# Reusable live-CLI probe runner for the agentic-RAG unified plan (P2/P3/P4b/P5).
# Runs each probe N times through agent.exe, captures the per-turn TraceRecord
# (planner.intent, actual_runtime_form, retrieval enabled/hits/cited_chunk_ids,
# tool_calls, steps) and the redacted assistant reply, and emits a JSONL summary
# (one line per run) + a compact table. No secret values are printed.
#
# Credentials come from eval\.smoke_env.ps1 (LLM_API_KEY + STS service keys).
# Retrieval (qwen3) reuses LLM_API_KEY (ModelVerse) per CLAUDE.md.
param(
    [Parameter(Mandatory = $true)][string]$ProbesPath,
    [int]$Runs = 5,
    [string]$Tag = "agentic-rag",
    [string]$ReportPath = "",
    [string]$External = "1",            # COMPSHARE_EXTERNAL_KNOWLEDGE
    [string]$AgenticSearch = "",        # COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE ("" = unset/off)
    [string]$SkillExec = "",            # USE_SKILL_EXECUTOR ("" = off)
    [string]$KnowledgeQAAgentLoop = "", # COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP ("" = off=terminal RAG; "1" = agent-loop route)
    [string]$GroundedValidator = ""     # COMPSHARE_RAG_GROUNDED_VALIDATOR ("" = off)
)

$ErrorActionPreference = "Continue"
$OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::InputEncoding = [System.Text.Encoding]::UTF8

$repoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $repoRoot            # agent.exe inherits this CWD so deploy/kb/*.jsonl relative paths resolve
[Environment]::CurrentDirectory = $repoRoot
if (-not [IO.Path]::IsPathRooted($ProbesPath)) { $ProbesPath = Join-Path $repoRoot $ProbesPath }
$agentExe = Join-Path $repoRoot "agent.exe"
$config = Join-Path $repoRoot "deploy\conf\agent.yaml"
$smokeEnv = Join-Path $repoRoot "eval\.smoke_env.ps1"

if (Test-Path $smokeEnv) { . $smokeEnv }
if (-not $env:LLM_API_KEY) { Write-Host "LLM_API_KEY not set; cannot run live probe." -ForegroundColor Red; exit 1 }
if (-not (Test-Path $agentExe)) { Write-Host "agent.exe not found; run: go build -o agent.exe ./cmd" -ForegroundColor Red; exit 1 }
if (-not (Test-Path $config)) {
    # Clean checkout: deploy/conf/agent.yaml is gitignored and absent. Fall back to the
    # tracked agent.yaml.example, whose ${ENV_VAR} placeholders the config loader
    # resolves from the environment (eval\.smoke_env.ps1 supplies the keys). Mirrors the
    # diagnose-skill smoke harness so the probe runs on a fresh worktree.
    $exampleConfig = Join-Path $repoRoot "deploy\conf\agent.yaml.example"
    if (Test-Path $exampleConfig) {
        Write-Host "config not found: $config -> falling back to agent.yaml.example (placeholders from env)" -ForegroundColor Yellow
        $config = $exampleConfig
    } else {
        Write-Host "config not found: $config (and no agent.yaml.example)" -ForegroundColor Red; exit 1
    }
}

$env:COMPSHARE_PROJECT_ID = "org-cwy2qk"
$env:COMPSHARE_ENABLE_MUTATING_TOOLS = ""
$env:USE_INTENT_PLANNER_FOR = ""
$env:MYSQL_DSN = ""
$env:COMPSHARE_TRACE_ENABLED = "1"
$env:COMPSHARE_EXTERNAL_KNOWLEDGE = $External
$env:COMPSHARE_AGENTIC_SEARCH_KNOWLEDGE = $AgenticSearch
$env:USE_SKILL_EXECUTOR = $SkillExec
$env:COMPSHARE_KNOWLEDGE_QA_AGENT_LOOP = $KnowledgeQAAgentLoop
$env:COMPSHARE_RAG_GROUNDED_VALIDATOR = $GroundedValidator

$runId = Get-Date -Format "yyyyMMdd-HHmmss"
$baseDir = Join-Path $env:TEMP "compshare-$Tag-$runId"
New-Item -ItemType Directory -Force -Path $baseDir | Out-Null
if (-not $ReportPath) { $ReportPath = Join-Path $baseDir "summary.jsonl" }

function Redact-LiveText {
    param([string]$Text)
    if ($null -eq $Text) { return "" }
    $out = $Text -replace "uhost-[A-Za-z0-9]+", "<UHOST_ID>"
    $out = $out -replace "UCloud-CompShare-[A-Za-z0-9]+", "<ACCESS_TOKEN>"
    $out = $out -replace "token=[A-Za-z0-9%._~+/=-]+", "token=<ACCESS_TOKEN>"
    $out = $out -replace "\b\d{1,3}(?:\.\d{1,3}){3}\b", "<IP>"
    return $out
}
function Get-AssistantReply {
    param([string]$Transcript)
    $m = [regex]::Matches($Transcript, "(?s)Assistant>\s*(.*?)(?=\r?\nYou>|$)")
    if ($m.Count -eq 0) { return "" }
    return $m[$m.Count - 1].Groups[1].Value.Trim()
}
function Read-TraceRecord {
    param([string]$Dir)
    $records = @()
    Get-ChildItem -Path $Dir -Filter "agent-trace-*.jsonl" -Recurse -ErrorAction SilentlyContinue | ForEach-Object {
        Get-Content $_.FullName -Encoding UTF8 | Where-Object { $_.Trim() } | ForEach-Object {
            try { $records += ($_ | ConvertFrom-Json) } catch { }
        }
    }
    $planned = @($records | Where-Object { $null -ne $_.planner -and $_.planner.intent } | Sort-Object turn_index)
    if ($planned.Count -eq 0) { return $null }
    return $planned[$planned.Count - 1]
}

$probes = [IO.File]::ReadAllText($ProbesPath, [Text.Encoding]::UTF8) | ConvertFrom-Json
if (Test-Path $ReportPath) { Remove-Item $ReportPath -Force }

$agenticLabel = $AgenticSearch; if ([string]::IsNullOrEmpty($agenticLabel)) { $agenticLabel = "off" }
$skillLabel = $SkillExec; if ([string]::IsNullOrEmpty($skillLabel)) { $skillLabel = "off" }
$kqaLabel = $KnowledgeQAAgentLoop; if ([string]::IsNullOrEmpty($kqaLabel)) { $kqaLabel = "off" }
Write-Host "ext=$External agentic=$agenticLabel skillexec=$skillLabel kqa_agent_loop=$kqaLabel runs=$Runs" -ForegroundColor Yellow

foreach ($probe in $probes) {
    $question = [string]$probe.question
    Write-Host ""
    Write-Host ">>> [$($probe.id)] x$Runs : $(Redact-LiveText $question)" -ForegroundColor Cyan
    for ($i = 1; $i -le $Runs; $i++) {
        $runDir = Join-Path $baseDir ("$($probe.id)-run$i")
        New-Item -ItemType Directory -Force -Path $runDir | Out-Null
        $env:COMPSHARE_TRACE_DIR = $runDir
        $inputPath = Join-Path $runDir "stdin.txt"
        [IO.File]::WriteAllText($inputPath, "$question`nexit`n", (New-Object Text.UTF8Encoding $false))
        $transcript = (Get-Content $inputPath -Raw -Encoding UTF8) | & $agentExe cli -c $config 2>&1 | Out-String
        [IO.File]::WriteAllText((Join-Path $runDir "transcript.txt"), $transcript, (New-Object Text.UTF8Encoding $false))

        $rec = Read-TraceRecord $runDir
        $intent = ""; $arf = ""; $rEnabled = $false; $rHits = 0; $cited = @(); $retrievedChunks = @(); $weakEvidence = $false; $toolActions = @(); $stepTools = @()
        if ($null -ne $rec) {
            if ($rec.planner) { $intent = [string]$rec.planner.intent }
            $arf = [string]$rec.actual_runtime_form
            if ($rec.retrieval) {
                $rEnabled = [bool]$rec.retrieval.enabled
                $rHits = [int]$rec.retrieval.hits
                if ($rec.retrieval.cited_chunk_ids) { $cited = @($rec.retrieval.cited_chunk_ids) }
                if ($rec.retrieval.hit_items) { $retrievedChunks = @($rec.retrieval.hit_items | ForEach-Object { [string]$_.chunk_id } | Where-Object { $_ }) }
                $weakEvidence = [bool]$rec.retrieval.weak_evidence
            }
            if ($rec.tool_calls) { $toolActions = @($rec.tool_calls | ForEach-Object { [string]$_.action }) }
            if ($rec.steps) { $stepTools = @($rec.steps | ForEach-Object { [string]$_.tool }) }
        }
        $reply = Get-AssistantReply $transcript
        $skFired = ($toolActions -contains "SearchKnowledge") -or ($stepTools -contains "SearchKnowledge") -or ($transcript -match "SearchKnowledge")
        $retrievalFired = ($rEnabled -and $rHits -gt 0)

        $redactedReply = Redact-LiveText $reply
        $row = [ordered]@{
            probe_id = $probe.id; run = $i; intent = $intent; actual_runtime_form = $arf
            retrieval_fired = $retrievalFired; retrieval_hits = $rHits; cited_chunk_ids = $cited
            retrieved_chunk_ids = $retrievedChunks; weak_evidence = $weakEvidence
            search_knowledge_fired = [bool]$skFired; tool_actions = $toolActions; step_tools = $stepTools
            reply_head = $redactedReply.Substring(0, [Math]::Min(160, $redactedReply.Length))
            reply_full = $redactedReply
        }
        ($row | ConvertTo-Json -Compress -Depth 6) | Add-Content -Path $ReportPath -Encoding UTF8
        Write-Host ("    run{0}: intent={1} form={2} retr={3}(h={4}) sk={5} got=[{6}] cited=[{7}]" -f $i, $intent, $arf, $retrievalFired, $rHits, $skFired, ($retrievedChunks -join ","), ($cited -join ",")) -ForegroundColor Gray
    }
}
Write-Host ""
Write-Host "Summary JSONL: $ReportPath" -ForegroundColor Green
Write-Host "Raw traces under: $baseDir" -ForegroundColor Green
