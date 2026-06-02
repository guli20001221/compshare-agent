param(
    [string]$UHostId = "",
    [string]$ImageName = "",
    [ValidateSet("all", "deny", "approve", "destructive")]
    [string]$Mode = "all",
    [string]$ReportPath = "",
    [switch]$PreflightOnly
)

$ErrorActionPreference = "Stop"
$OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::InputEncoding = [System.Text.Encoding]::UTF8

$repoRoot = Resolve-Path (Join-Path $PSScriptRoot "..")
Set-Location $repoRoot

function Mask-Id([string]$value) {
    if ([string]::IsNullOrWhiteSpace($value)) { return "" }
    if ($value.Length -le 8) { return "<redacted>" }
    return $value.Substring(0, 6) + "-<redacted>"
}

function Require-Env([string]$name) {
    $value = [Environment]::GetEnvironmentVariable($name)
    if ([string]::IsNullOrWhiteSpace($value)) {
        throw "$name is required for this live smoke"
    }
    return $value
}

function Read-TraceRecords([string]$dir) {
    $records = @()
    Get-ChildItem $dir -Filter "*.jsonl" -Recurse -ErrorAction SilentlyContinue |
        Sort-Object LastWriteTime |
        ForEach-Object {
            Get-Content $_.FullName -Encoding UTF8 |
                Where-Object { -not [string]::IsNullOrWhiteSpace($_) } |
                ForEach-Object {
                    try { $records += ($_ | ConvertFrom-Json) } catch {}
                }
        }
    return $records
}

function Trace-ToolCalls($records) {
    $calls = @()
    foreach ($record in $records) {
        if ($record.tool_calls) {
            foreach ($call in $record.tool_calls) {
                $calls += [string]$call.action
            }
        }
    }
    return $calls
}

function Run-AgentCase([string]$name, [string]$inputText) {
    $caseDir = Join-Path $script:traceRoot $name
    New-Item -ItemType Directory -Force $caseDir | Out-Null
    $env:COMPSHARE_TRACE_DIR = $caseDir

    $transcript = Join-Path $caseDir "transcript.txt"
    $inputText | .\agent.exe cli -c .\deploy\conf\agent.yaml 2>&1 |
        Tee-Object -FilePath $transcript | Out-Null

    $records = Read-TraceRecords $caseDir
    $toolCalls = Trace-ToolCalls $records
    $rawTranscript = Get-Content $transcript -Raw -Encoding UTF8

    return [pscustomobject]@{
        Name = $name
        TraceDir = $caseDir
        TranscriptPath = $transcript
        ToolCalls = $toolCalls
        MentionsMissingUserEmail = ($rawTranscript -match "Missing params \\[user_email\\]")
        MentionsCreate = ($toolCalls -contains "CreateCompShareCustomImage")
        MentionsTerminate = ($toolCalls -contains "TerminateCompShareInstance")
    }
}

function Assert-SmokeResult($result) {
    switch ($result.Name) {
        "deny" {
            if ($result.MentionsCreate) {
                throw "deny leg called CreateCompShareCustomImage"
            }
        }
        "approve" {
            if (-not $result.MentionsCreate) {
                throw "approve leg did not call CreateCompShareCustomImage"
            }
            if ($result.MentionsMissingUserEmail) {
                throw "approve leg still failed with Missing params [user_email]"
            }
        }
        "destructive" {
            if ($result.MentionsTerminate) {
                throw "destructive leg called TerminateCompShareInstance"
            }
        }
    }
}

function Write-Report($results, [string]$path) {
    $lines = @()
    $lines += "# Custom Image user_email Live Smoke Report"
    $lines += ""
    $lines += "Date: $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')"
    $lines += "Project: $env:COMPSHARE_PROJECT_ID"
    $lines += "UHostId: $(Mask-Id $UHostId)"
    $lines += "ImageName: $ImageName"
    $lines += "COMPSHARE_USER_EMAIL set: $([string]::IsNullOrWhiteSpace($env:COMPSHARE_USER_EMAIL) -eq $false)"
    $lines += "Trace root: $script:traceRoot"
    $lines += ""
    $lines += "| case | tool calls | missing user_email | pass condition |"
    $lines += "| --- | --- | --- | --- |"
    foreach ($result in $results) {
        $tools = if ($result.ToolCalls.Count -gt 0) { $result.ToolCalls -join "," } else { "(none)" }
        $condition = switch ($result.Name) {
            "deny" { "no CreateCompShareCustomImage" }
            "approve" { "CreateCompShareCustomImage reached and no Missing params [user_email]" }
            "destructive" { "no TerminateCompShareInstance" }
            default { "" }
        }
        $lines += "| $($result.Name) | $tools | $($result.MentionsMissingUserEmail) | $condition |"
    }
    $lines += ""
    $lines += "Raw transcripts and trace JSONL stay local under the trace root."
    [IO.File]::WriteAllLines($path, $lines, [Text.UTF8Encoding]::new($false))
}

$smokeEnv = Join-Path $repoRoot "eval\.smoke_env.ps1"
if (Test-Path $smokeEnv) {
    . $smokeEnv
}

if ([string]::IsNullOrWhiteSpace($env:COMPSHARE_PROJECT_ID)) {
    $env:COMPSHARE_PROJECT_ID = "org-cwy2qk"
}

if ($PreflightOnly) {
    [pscustomobject]@{
        SmokeEnvExists = (Test-Path $smokeEnv)
        AgentConfigExists = (Test-Path (Join-Path $repoRoot "deploy\conf\agent.yaml"))
        CompshareProjectIDSet = (-not [string]::IsNullOrWhiteSpace($env:COMPSHARE_PROJECT_ID))
        CompshareUserEmailSet = (-not [string]::IsNullOrWhiteSpace($env:COMPSHARE_USER_EMAIL))
    } | ConvertTo-Json -Depth 4
    exit 0
}

Require-Env "COMPSHARE_USER_EMAIL" | Out-Null
if ([string]::IsNullOrWhiteSpace($UHostId)) {
    throw "UHostId is required for live custom-image smoke"
}
if ([string]::IsNullOrWhiteSpace($ImageName)) {
    $ImageName = "claude-smoke-image-" + (Get-Date -Format "yyyyMMdd-HHmmss")
}

$env:COMPSHARE_ENABLE_MUTATING_TOOLS = "1"
$env:COMPSHARE_TRACE_ENABLED = "1"
$runId = Get-Date -Format "yyyyMMdd-HHmmss"
$script:traceRoot = Join-Path $env:TEMP "compshare-custom-image-user-email-$runId"
New-Item -ItemType Directory -Force $script:traceRoot | Out-Null

go build -o agent.exe ./cmd | Out-Null

$results = @()
if ($Mode -eq "all" -or $Mode -eq "deny") {
    $results += Run-AgentCase "deny" "把 $UHostId 保存成自定义镜像，名字叫 $ImageName`nN`nexit"
}
if ($Mode -eq "all" -or $Mode -eq "approve") {
    $results += Run-AgentCase "approve" "把 $UHostId 保存成自定义镜像，名字叫 $ImageName`ny`nexit"
}
if ($Mode -eq "all" -or $Mode -eq "destructive") {
    $results += Run-AgentCase "destructive" "销毁 $UHostId`ny`nexit"
}

foreach ($result in $results) {
    Assert-SmokeResult $result
}

if ([string]::IsNullOrWhiteSpace($ReportPath)) {
    $ReportPath = Join-Path $script:traceRoot "custom_image_user_email_smoke_report.md"
}
Write-Report $results $ReportPath
Write-Host "report: $ReportPath"
