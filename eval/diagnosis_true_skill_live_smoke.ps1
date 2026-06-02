# Live CLI smoke for the body-read diagnosis skill path.
# Credentials are sourced from eval\.smoke_env.ps1; no secret values are printed.
param(
    [int]$Runs = 3,
    [string]$Tag = "diagnosis-true-skill",
    [string]$CasesPath = "",
    [string]$ReportPath = "",
    [string]$SkillExec = "1",
    [string]$SkillAllowlist = "diagnose_port_firewall",
    [string]$UHostId = ""
)

$ErrorActionPreference = "Continue"
$OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::InputEncoding = [System.Text.Encoding]::UTF8

$repoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
if (-not $CasesPath) {
    $CasesPath = Join-Path $repoRoot "eval\diagnosis_true_skill_live_cases.json"
}
$agentExe = Join-Path $repoRoot "agent.exe"
$config = Join-Path $repoRoot "deploy\conf\agent.yaml"
$smokeEnv = Join-Path $repoRoot "eval\.smoke_env.ps1"

if (Test-Path $smokeEnv) {
    . $smokeEnv
}
if (-not $env:LLM_API_KEY) {
    Write-Host "LLM_API_KEY is not set; cannot run live diagnosis smoke." -ForegroundColor Red
    exit 1
}
if (-not (Test-Path $agentExe)) {
    Write-Host "agent.exe not found; run: go build -o agent.exe ./cmd" -ForegroundColor Red
    exit 1
}
if (-not $UHostId) {
    $UHostId = $env:DIAGNOSIS_SMOKE_UHOST_ID
}

$cases = [IO.File]::ReadAllText($CasesPath, [Text.Encoding]::UTF8) | ConvertFrom-Json
$needsUHost = $false
foreach ($case in $cases) {
    if ([string]$case.question -like "*{{UHOST_ID}}*") {
        $needsUHost = $true
    }
}
if ($needsUHost -and -not $UHostId) {
    Write-Host "A case needs {{UHOST_ID}}. Pass -UHostId or set DIAGNOSIS_SMOKE_UHOST_ID." -ForegroundColor Red
    exit 1
}

$env:COMPSHARE_PROJECT_ID = "org-cwy2qk"
$env:COMPSHARE_ENABLE_MUTATING_TOOLS = ""
$env:USE_INTENT_PLANNER_FOR = ""
$skillExecEnv = $SkillExec
if ($SkillExec -eq "0" -or $SkillExec -eq "off") {
    $skillExecEnv = ""
}
$env:USE_SKILL_EXECUTOR = $skillExecEnv
$env:USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS = $SkillAllowlist
$env:MYSQL_DSN = ""
$env:COMPSHARE_TRACE_ENABLED = "1"

$runId = Get-Date -Format "yyyyMMdd-HHmmss"
$baseDir = Join-Path $env:TEMP "compshare-diagnosis-true-skill-$Tag-$runId"
New-Item -ItemType Directory -Force -Path $baseDir | Out-Null

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
    $matches = [regex]::Matches($Transcript, "(?s)Assistant>\s*(.*?)(?=\r?\nYou>|$)")
    if ($matches.Count -eq 0) { return "" }
    return $matches[$matches.Count - 1].Groups[1].Value.Trim()
}

function Test-MutatingAction {
    param([string]$Action)
    return $Action -match "^(Create|Attach|Detach|Delete|Terminate|Destroy|Stop|Start|Reboot|Resize|Publish|Unpublish|Modify|Update)"
}

function Read-TraceRecord {
    param([string]$Dir)
    $records = @()
    Get-ChildItem -Path $Dir -Filter "agent-trace-*.jsonl" -Recurse -ErrorAction SilentlyContinue | ForEach-Object {
        Get-Content $_.FullName -Encoding UTF8 | Where-Object { $_.Trim() } | ForEach-Object {
            try {
                $records += ($_ | ConvertFrom-Json)
            } catch {
            }
        }
    }
    $planned = @($records | Where-Object { $null -ne $_.planner -and $_.planner.intent } | Sort-Object turn_index)
    if ($planned.Count -eq 0) { return $null }
    return $planned[$planned.Count - 1]
}

function Contains-All {
    param($Actual, $Expected)
    foreach ($item in @($Expected)) {
        if (@($Actual | Where-Object { $_ -eq $item }).Count -eq 0) {
            return $false
        }
    }
    return $true
}

$results = @()

foreach ($case in $cases) {
    $caseResults = @()
    $questionTemplate = [string]$case.question
    $question = $questionTemplate.Replace("{{UHOST_ID}}", $UHostId)

    Write-Host ""
    Write-Host ">>> [$($case.id)] skillExec=$SkillExec x$Runs" -ForegroundColor Cyan
    Write-Host "    $(Redact-LiveText $question)" -ForegroundColor Gray

    for ($i = 1; $i -le $Runs; $i++) {
        $runDir = Join-Path $baseDir ("$($case.id)-run$i")
        New-Item -ItemType Directory -Force -Path $runDir | Out-Null
        $env:COMPSHARE_TRACE_DIR = $runDir

        $inputPath = Join-Path $runDir "stdin.txt"
        [IO.File]::WriteAllText($inputPath, "$question`nexit`n", (New-Object Text.UTF8Encoding $false))
        $transcript = (Get-Content $inputPath -Raw -Encoding UTF8) | & $agentExe cli -c $config 2>&1 | Out-String
        $transcript | Set-Content -Path (Join-Path $runDir "transcript.txt") -Encoding UTF8
        Write-Host -NoNewline "."

        $record = Read-TraceRecord $runDir
        $reply = Get-AssistantReply $transcript
        $expectedTools = @($case.expect_tools)
        if ($SkillExec -ne "1" -and $null -ne $case.expect_tools_off) {
            $expectedTools = @($case.expect_tools_off)
        }
        $expectRetrieval = [bool]$case.expect_retrieval
        if ($SkillExec -ne "1" -and $null -ne $case.expect_retrieval_off) {
            $expectRetrieval = [bool]$case.expect_retrieval_off
        }
        $requireBodyRead = [bool]$case.require_body_read
        if ($SkillExec -ne "1" -and $null -ne $case.require_body_read_off) {
            $requireBodyRead = [bool]$case.require_body_read_off
        }
        $expectedDiagnosisTool = [string]$case.expect_diagnosis_tool
        if (-not $expectedDiagnosisTool) {
            if ([string]$case.expect_skill -eq "diagnose_port_firewall") {
                $expectedDiagnosisTool = "DiagnosePortOrFirewall"
            } elseif ([string]$case.expect_skill -eq "diagnose_gpu_not_detected") {
                $expectedDiagnosisTool = "DiagnoseGPU"
            }
        }
        $toolActions = @()
        $toolSources = @{}
        if ($record -and $record.tool_calls) {
            foreach ($call in @($record.tool_calls)) {
                $toolActions += [string]$call.action
                $toolSources[[string]$call.action] = [string]$call.source
            }
        }

        $mutating = @($toolActions | Where-Object { Test-MutatingAction $_ })
        $diagnosisActions = @($toolActions | Where-Object { $_ -like "Diagnose*" })
        $retrievalEnabled = $false
        $retrievalHits = 0
        if ($record -and $record.retrieval) {
            $retrievalEnabled = [bool]$record.retrieval.enabled
            $retrievalHits = [int]$record.retrieval.hits
        }

        $forbiddenHits = @()
        foreach ($forbidden in @($case.forbidden_text)) {
            if ($forbidden -and $reply.Contains([string]$forbidden)) {
                $forbiddenHits += [string]$forbidden
            }
        }

        $replyContainsToken = $reply -match "UCloud-CompShare-[A-Za-z0-9]+|token=[A-Za-z0-9%._~+/=-]+"
        # Keep this ASCII-only for Windows PowerShell 5.1. The boundary check is
        # structural: a no-target diagnosis should answer with a clarification
        # instead of running the diagnosis skill or retrieval path.
        $clarified = ($reply.Trim().Length -gt 0 -and $diagnosisActions.Count -eq 0 -and @($toolActions | Where-Object { $_ -eq "SearchKnowledge" }).Count -eq 0 -and $mutating.Count -eq 0)

        $checks = [ordered]@{}
        $checks.intent = ($record -and ([string]$record.planner.intent -eq [string]$case.expect_intent))
        $checks.expected_tools = Contains-All $toolActions $expectedTools
        $checks.no_mutating = (-not [bool]$case.forbid_mutating) -or ($mutating.Count -eq 0)
        $checks.no_diagnosis = (-not [bool]$case.forbid_diagnosis) -or ($diagnosisActions.Count -eq 0 -and [string]$record.planner.intent -ne "diagnosis")
        $checks.retrieval = (-not $expectRetrieval) -or ($retrievalEnabled -and $retrievalHits -gt 0 -or @($toolActions | Where-Object { $_ -eq "SearchKnowledge" }).Count -gt 0)
        $checks.no_unexpected_retrieval = ($expectRetrieval) -or (@($toolActions | Where-Object { $_ -eq "SearchKnowledge" }).Count -eq 0)
        $checks.body_read = (-not $requireBodyRead) -or (@($toolActions | Where-Object { $_ -eq "SearchKnowledge" }).Count -gt 0 -and @($toolActions | Where-Object { $_ -eq $expectedDiagnosisTool }).Count -gt 0)
        $checks.clarify = (-not [bool]$case.expect_clarify) -or $clarified
        $checks.no_forbidden_text = ($forbiddenHits.Count -eq 0)

        $pass = $true
        foreach ($value in $checks.Values) {
            if (-not $value) { $pass = $false }
        }

        $caseResults += [PSCustomObject]@{
            run = $i
            pass = $pass
            intent = if ($record) { [string]$record.planner.intent } else { "" }
            planned_runtime_form = if ($record) { [string]$record.planner.planned_runtime_form } else { "" }
            actual_runtime_form = if ($record) { [string]$record.actual_runtime_form } else { "" }
            cutover_status = if ($record) { [string]$record.planner.cutover_status } else { "" }
            tool_actions = $toolActions
            tool_sources = $toolSources
            retrieval_enabled = $retrievalEnabled
            retrieval_hits = $retrievalHits
            expected_tools = $expectedTools
            expected_diagnosis_tool = $expectedDiagnosisTool
            expect_retrieval = $expectRetrieval
            require_body_read = $requireBodyRead
            mutating_actions = $mutating
            diagnosis_actions = $diagnosisActions
            forbidden_text_hits = $forbiddenHits
            reply_contains_access_token = $replyContainsToken
            redacted_reply_preview = (Redact-LiveText $reply).Substring(0, [Math]::Min(500, (Redact-LiveText $reply).Length))
            checks = $checks
            trace_dir = $runDir
        }
    }
    Write-Host ""

    $results += [PSCustomObject]@{
        id = [string]$case.id
        question = Redact-LiveText $question
        expect_intent = [string]$case.expect_intent
        expect_skill = [string]$case.expect_skill
        runs = $Runs
        pass = (@($caseResults | Where-Object { -not $_.pass }).Count -eq 0)
        results = $caseResults
    }
}

$summary = [PSCustomObject]@{
    tag = $Tag
    skill_exec = $SkillExec
    skill_allowlist = $SkillAllowlist
    runs_per_case = $Runs
    trace_dir = $baseDir
    pass = (@($results | Where-Object { -not $_.pass }).Count -eq 0)
    used_redacted_uhost_id = $needsUHost
    results = $results
}

$json = $summary | ConvertTo-Json -Depth 20
if ($ReportPath) {
    $json | Set-Content -Path $ReportPath -Encoding UTF8
}
$json
