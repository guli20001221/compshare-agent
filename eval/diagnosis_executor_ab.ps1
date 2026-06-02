# Live A/B gate for deciding whether the diagnosis skill executor can be
# widened or defaulted on. The script stores redacted reports only.
param(
    [int]$Runs = 5,
    [string]$CasesPath = "",
    [string]$ReportPath = "",
    [string]$UHostId = "",
    [string]$Tag = "diagnosis-executor-ab",
    [switch]$FailOnGate
)

$ErrorActionPreference = "Continue"
$OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::InputEncoding = [System.Text.Encoding]::UTF8

$repoRoot = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
if (-not $CasesPath) {
    $CasesPath = Join-Path $repoRoot "eval\diagnosis_executor_ab_cases.json"
}
if (-not $ReportPath) {
    $ReportPath = Join-Path $env:TEMP "diagnosis-executor-ab.json"
}

$agentExe = Join-Path $repoRoot "agent.exe"
$config = Join-Path $repoRoot "deploy\conf\agent.yaml"
$smokeEnv = Join-Path $repoRoot "eval\.smoke_env.ps1"

if (Test-Path $smokeEnv) {
    . $smokeEnv
}
if (-not $env:LLM_API_KEY) {
    Write-Host "LLM_API_KEY is not set; cannot run live A/B gate." -ForegroundColor Red
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

$configs = @(
    [PSCustomObject]@{ name = "off"; skill_exec = ""; allowlist = "" },
    [PSCustomObject]@{ name = "on_port_only"; skill_exec = "1"; allowlist = "diagnose_port_firewall" },
    [PSCustomObject]@{ name = "on_port_gpu"; skill_exec = "1"; allowlist = "diagnose_port_firewall,diagnose_gpu_not_detected" }
)

$env:COMPSHARE_PROJECT_ID = "org-cwy2qk"
$env:COMPSHARE_ENABLE_MUTATING_TOOLS = ""
$env:USE_INTENT_PLANNER_FOR = ""
$env:MYSQL_DSN = ""
$env:COMPSHARE_TRACE_ENABLED = "1"

$runId = Get-Date -Format "yyyyMMdd-HHmmss"
$baseDir = Join-Path $env:TEMP "compshare-diagnosis-ab-$Tag-$runId"
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

function Contains-Any {
    param([string]$Text, $Terms)
    $items = @($Terms)
    if ($items.Count -eq 0) { return $true }
    foreach ($item in $items) {
        if ($item -and $Text.Contains([string]$item)) {
            return $true
        }
    }
    return $false
}

function Contains-Skill {
    param([string]$Allowlist, [string]$Skill)
    if (-not $Skill) { return $false }
    foreach ($item in $Allowlist.Split(",")) {
        if ($item.Trim() -eq $Skill) { return $true }
    }
    return $false
}

$allResults = @()

foreach ($cfg in $configs) {
    $env:USE_SKILL_EXECUTOR = $cfg.skill_exec
    $env:USE_SKILL_EXECUTOR_DIAGNOSIS_SKILLS = $cfg.allowlist

    foreach ($case in $cases) {
        $questionTemplate = [string]$case.question
        $question = $questionTemplate.Replace("{{UHOST_ID}}", $UHostId)

        Write-Host ""
        Write-Host ">>> [$($cfg.name)] [$($case.id)] x$Runs" -ForegroundColor Cyan
        Write-Host "    $(Redact-LiveText $question)" -ForegroundColor Gray

        for ($i = 1; $i -le $Runs; $i++) {
            $runDir = Join-Path $baseDir "$($cfg.name)-$($case.id)-run$i"
            New-Item -ItemType Directory -Force -Path $runDir | Out-Null
            $env:COMPSHARE_TRACE_DIR = $runDir

            $inputPath = Join-Path $runDir "stdin.txt"
            [IO.File]::WriteAllText($inputPath, "$question`nexit`n", (New-Object Text.UTF8Encoding $false))
            $started = Get-Date
            $transcript = (Get-Content $inputPath -Raw -Encoding UTF8) | & $agentExe cli -c $config 2>&1 | Out-String
            $elapsedMs = [int]((Get-Date) - $started).TotalMilliseconds
            $transcript | Set-Content -Path (Join-Path $runDir "transcript.txt") -Encoding UTF8
            Write-Host -NoNewline "."

            $record = Read-TraceRecord $runDir
            $reply = Get-AssistantReply $transcript
            $toolActions = @()
            if ($record -and $record.tool_calls) {
                foreach ($call in @($record.tool_calls)) {
                    $toolActions += [string]$call.action
                }
            }

            $mutating = @($toolActions | Where-Object { Test-MutatingAction $_ })
            $diagnosisActions = @($toolActions | Where-Object { $_ -like "Diagnose*" })
            $searchCalls = @($toolActions | Where-Object { $_ -eq "SearchKnowledge" })
            $expectedAction = [string]$case.expect_action
            $expectedSkill = [string]$case.expect_skill
            $expectedBodyRead = ($cfg.skill_exec -eq "1" -and (Contains-Skill $cfg.allowlist $expectedSkill) -and ($expectedSkill -eq "diagnose_port_firewall" -or $expectedSkill -eq "diagnose_gpu_not_detected"))

            $forbiddenHits = @()
            foreach ($forbidden in @($case.forbidden_text)) {
                if ($forbidden -and $reply.Contains([string]$forbidden)) {
                    $forbiddenHits += [string]$forbidden
                }
            }

            $intentHit = ($record -and ([string]$record.planner.intent -eq [string]$case.expect_intent))
            $actionHit = (-not $expectedAction) -or (@($toolActions | Where-Object { $_ -eq $expectedAction }).Count -gt 0)
            $noRawLeak = ($forbiddenHits.Count -eq 0)
            $noMutating = (-not [bool]$case.forbid_mutating) -or ($mutating.Count -eq 0)
            $controlOk = (-not [bool]$case.forbid_diagnosis) -or ($diagnosisActions.Count -eq 0 -and [string]$record.planner.intent -ne "diagnosis")
            $noUnexpectedTools = (-not [bool]$case.expect_no_tools) -or ($toolActions.Count -eq 0)
            $qualityOk = Contains-Any $reply @($case.quality_terms)
            $bodyReadOk = (-not $expectedBodyRead) -or ($searchCalls.Count -gt 0 -and $actionHit)
            $replyContainsAccessToken = $reply -match "UCloud-CompShare-[A-Za-z0-9]+|token=[A-Za-z0-9%._~+/=-]+"

            $latencyMs = $elapsedMs
            $tokens = 0
            if ($record -and $record.outcome) {
                if ($record.outcome.total_latency_ms) { $latencyMs = [int]$record.outcome.total_latency_ms }
                if ($record.outcome.total_tokens) { $tokens = [int]$record.outcome.total_tokens }
            }

            $failClosed = $false
            if ($record -and $record.tool_calls) {
                foreach ($call in @($record.tool_calls)) {
                    $err = ([string]$call.error_class).ToLowerInvariant()
                    if ($err.Contains("raw") -or $err.Contains("leak") -or $err.Contains("evidence")) {
                        $failClosed = $true
                    }
                }
            }

            $processSuccess = ($intentHit -and $actionHit -and $noRawLeak -and $noMutating -and $controlOk -and $noUnexpectedTools -and $qualityOk -and $bodyReadOk)
            $checks = [ordered]@{
                intent = $intentHit
                expected_action = $actionHit
                no_raw_leak = $noRawLeak
                no_mutating = $noMutating
                control_no_diagnosis = $controlOk
                no_unexpected_tools = $noUnexpectedTools
                answer_quality = $qualityOk
                expected_body_read = $bodyReadOk
            }

            $allResults += [PSCustomObject]@{
                config = $cfg.name
                skill_exec = $cfg.skill_exec
                allowlist = $cfg.allowlist
                case_id = [string]$case.id
                run = $i
                question = Redact-LiveText $question
                intent = if ($record) { [string]$record.planner.intent } else { "" }
                planned_runtime_form = if ($record) { [string]$record.planner.planned_runtime_form } else { "" }
                actual_runtime_form = if ($record) { [string]$record.actual_runtime_form } else { "" }
                cutover_status = if ($record) { [string]$record.planner.cutover_status } else { "" }
                tool_actions = $toolActions
                mutating_actions = $mutating
                diagnosis_actions = $diagnosisActions
                search_knowledge_calls = $searchCalls.Count
                expected_action = $expectedAction
                expected_skill = $expectedSkill
                expected_body_read = $expectedBodyRead
                fail_closed = $failClosed
                forbidden_text_hits = $forbiddenHits
                reply_contains_access_token = $replyContainsAccessToken
                redacted_reply_preview = (Redact-LiveText $reply).Substring(0, [Math]::Min(420, (Redact-LiveText $reply).Length))
                latency_ms = $latencyMs
                total_tokens = $tokens
                process_success = $processSuccess
                checks = $checks
                trace_dir = $runDir
            }
        }
        Write-Host ""
    }
}

$aggregates = @()
foreach ($cfg in $configs) {
    $rows = @($allResults | Where-Object { $_.config -eq $cfg.name })
    $count = [Math]::Max(1, $rows.Count)
    $intentHits = @($rows | Where-Object { $_.checks.intent }).Count
    $actionHits = @($rows | Where-Object { $_.checks.expected_action }).Count
    $processHits = @($rows | Where-Object { $_.process_success }).Count
    $rawLeaks = @($rows | Where-Object { @($_.forbidden_text_hits).Count -gt 0 }).Count
    $mutating = 0
    foreach ($row in $rows) { $mutating += @($row.mutating_actions).Count }
    $controlMisroutes = @($rows | Where-Object { $_.case_id -like "*control*" -and -not $_.checks.control_no_diagnosis }).Count
    $noTargetExtraTools = @($rows | Where-Object { ($_.case_id -like "*no_target*") -and -not $_.checks.no_unexpected_tools }).Count
    $bodyReadMisses = @($rows | Where-Object { $_.expected_body_read -and -not $_.checks.expected_body_read }).Count
    $accessTokenReplies = @($rows | Where-Object { $_.reply_contains_access_token }).Count
    $failClosed = @($rows | Where-Object { $_.fail_closed }).Count
    $avgLatency = [int](($rows | Measure-Object -Property latency_ms -Average).Average)
    $avgTokens = [int](($rows | Measure-Object -Property total_tokens -Average).Average)

    $aggregates += [PSCustomObject]@{
        config = $cfg.name
        runs = $rows.Count
        intent_hit_rate = [Math]::Round($intentHits / $count, 4)
        expected_action_rate = [Math]::Round($actionHits / $count, 4)
        process_success_rate = [Math]::Round($processHits / $count, 4)
        raw_evidence_leak_count = $rawLeaks
        mutating_tool_call_count = $mutating
        control_misroute_count = $controlMisroutes
        no_target_extra_tool_count = $noTargetExtraTools
        body_read_miss_count = $bodyReadMisses
        access_token_reply_count = $accessTokenReplies
        fail_closed_count = $failClosed
        avg_latency_ms = $avgLatency
        avg_total_tokens = $avgTokens
    }
}

$off = @($aggregates | Where-Object { $_.config -eq "off" })[0]
$onAll = @($aggregates | Where-Object { $_.config -eq "on_port_gpu" })[0]
$defaultOnRecommended = (
    $onAll.raw_evidence_leak_count -eq 0 -and
    $onAll.mutating_tool_call_count -eq 0 -and
    $onAll.control_misroute_count -eq 0 -and
    $onAll.body_read_miss_count -eq 0 -and
    $onAll.no_target_extra_tool_count -eq 0 -and
    $onAll.access_token_reply_count -eq 0 -and
    $onAll.process_success_rate -ge $off.process_success_rate
)

$summary = [PSCustomObject]@{
    tag = $Tag
    runs_per_case_per_config = $Runs
    trace_dir = $baseDir
    used_redacted_uhost_id = $needsUHost
    default_on_recommended = $defaultOnRecommended
    decision_basis = "Conservative gate: no raw leaks, no mutating calls, no control misroutes, no body-read misses, no no-target extra tools, no access-token replies, and on-all process success >= off."
    aggregates = $aggregates
    results = $allResults
}

$json = $summary | ConvertTo-Json -Depth 20
$json | Set-Content -Path $ReportPath -Encoding UTF8
$json

if ($FailOnGate -and -not $defaultOnRecommended) {
    exit 1
}
