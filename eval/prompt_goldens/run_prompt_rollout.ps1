param(
    [string]$CasesPath = "",
    [string]$Tag = "prompt-cardization",
    [ValidateSet("0", "1")][string]$Mutating = "1",
    [switch]$SkipBuild,
    [string]$ReportPath = ""
)

$ErrorActionPreference = "Stop"
$OutputEncoding = [System.Text.Encoding]::UTF8
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

$repoRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
if (-not $CasesPath) { $CasesPath = Join-Path $PSScriptRoot "cases.json" }
$runner = Join-Path $repoRoot "eval\context_prompt_cli_regression.ps1"
$runId = Get-Date -Format "yyyyMMdd-HHmmss"
$baseDir = Join-Path $env:TEMP "compshare-$Tag-rollout-$runId"
New-Item -ItemType Directory -Force -Path $baseDir | Out-Null
if (-not $ReportPath) { $ReportPath = Join-Path $baseDir "rollout_report.json" }

$baselineReport = Join-Path $baseDir "baseline.json"
$candidateReport = Join-Path $baseDir "intent_scoped.json"

$commonArgs = @("-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $runner, "-CasesPath", $CasesPath, "-Mutating", $Mutating)
if ($SkipBuild) { $commonArgs += "-SkipBuild" }

& powershell @commonArgs -Tag "$Tag-baseline" -ReportPath $baselineReport
if ($LASTEXITCODE -ne 0) { throw "baseline run failed with exit code $LASTEXITCODE" }

& powershell @commonArgs -Tag "$Tag-intent-scoped" -EnableIntentScopedReActPrompt -ReportPath $candidateReport
if ($LASTEXITCODE -ne 0) { throw "intent-scoped run failed with exit code $LASTEXITCODE" }

$baseline = [IO.File]::ReadAllText($baselineReport, [Text.Encoding]::UTF8) | ConvertFrom-Json
$candidate = [IO.File]::ReadAllText($candidateReport, [Text.Encoding]::UTF8) | ConvertFrom-Json

$baselinePrompt = [int]$baseline.token_totals.prompt_tokens
$candidatePrompt = [int]$candidate.token_totals.prompt_tokens
$delta = $candidatePrompt - $baselinePrompt

$summary = [PSCustomObject]@{
    tag = $Tag
    cases_path = $CasesPath
    baseline_report = $baselineReport
    candidate_report = $candidateReport
    baseline_pass = [bool]$baseline.pass
    candidate_pass = [bool]$candidate.pass
    baseline_prompt_tokens = $baselinePrompt
    candidate_prompt_tokens = $candidatePrompt
    prompt_token_delta = $delta
    prompt_token_reduced = ($candidatePrompt -gt 0 -and $baselinePrompt -gt 0 -and $candidatePrompt -lt $baselinePrompt)
    pass = ([bool]$baseline.pass -and [bool]$candidate.pass -and ($candidatePrompt -gt 0) -and ($candidatePrompt -lt $baselinePrompt))
}

$summary | ConvertTo-Json -Depth 8 | Set-Content -Path $ReportPath -Encoding UTF8
$summary | ConvertTo-Json -Depth 8
if (-not $summary.pass) {
    exit 1
}
