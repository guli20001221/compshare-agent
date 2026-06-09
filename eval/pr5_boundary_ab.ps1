# PR5 stock-vs-resource BoundaryPack A/B parity harness.
#
# Runs each anchor/jitter question N times through one agent binary, reads
# planner.intent from the per-question trace, and reports the intent spread +
# whether the stable intent matches the expected one. Run it once per binary
# (OLD = base-prompt baseline, NEW = boundary-pack) and diff the per-question
# intents to prove the directive's relocation did not change classification.
#
# Encoding handling mirrors eval/planner_jitter.ps1 (the proven harness): UTF-8
# stdin without BOM, UTF-8 console, intent field is ASCII so trace parse is safe.
# Needs only LLM_API_KEY (planner is a pure LLM call). Never prints secrets.
#
# Usage:
#   pwsh -File eval\pr5_boundary_ab.ps1 -AgentExe <path> -Config <agent.yaml> `
#        -QuestionsPath <json> -OutDir <dir> -Label <old|new> [-Runs 5]

param(
    [Parameter(Mandatory = $true)][string]$AgentExe,
    [Parameter(Mandatory = $true)][string]$Config,
    [Parameter(Mandatory = $true)][string]$QuestionsPath,
    [Parameter(Mandatory = $true)][string]$OutDir,
    [Parameter(Mandatory = $true)][string]$Label,
    [int]$Runs = 5
)

$ErrorActionPreference = "Continue"
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$OutputEncoding = [Text.Encoding]::UTF8
[Console]::InputEncoding = [Text.Encoding]::UTF8

# Source ONLY the $env: lines from start-server.ps1 (LLM_API_KEY + optional STS).
$startServer = "F:\compshare-agent\start-server.ps1"
if (Test-Path $startServer) {
    Get-Content $startServer | Where-Object { $_ -match '^\$env:' } | ForEach-Object { Invoke-Expression $_ }
}
if (-not $env:LLM_API_KEY) {
    Write-Host "LLM_API_KEY not set - aborting." -ForegroundColor Red
    exit 1
}
# The local agent.yaml hardcodes project_id: "${COMPSHARE_PROJECT_ID}" (no
# default syntax), so config load requires the var even though the planner is a
# pure pre-API LLM call whose classification is independent of project_id.
# Default to a placeholder when the ambient env / start-server.ps1 omit it.
if (-not $env:COMPSHARE_PROJECT_ID) { $env:COMPSHARE_PROJECT_ID = "test-project" }

$env:COMPSHARE_ENABLE_MUTATING_TOOLS = "0"
$env:MYSQL_DSN = ""
$env:COMPSHARE_TRACE_ENABLED = "1"

if (-not (Test-Path $AgentExe)) { Write-Host "agent binary not found: $AgentExe" -ForegroundColor Red; exit 1 }
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

$questions = [IO.File]::ReadAllText($QuestionsPath, [Text.Encoding]::UTF8) | ConvertFrom-Json
$report = @()

foreach ($q in $questions) {
    $qDir = Join-Path $OutDir "$($q.qid)_$Label"
    if (Test-Path $qDir) { Remove-Item $qDir -Recurse -Force }
    New-Item -ItemType Directory -Force -Path $qDir | Out-Null
    $env:COMPSHARE_TRACE_DIR = $qDir

    Write-Host ">>> [$Label][$($q.qid)] expect=$($q.expect)  Q: $($q.question)" -ForegroundColor Cyan
    Write-Host -NoNewline "    "
    for ($i = 1; $i -le $Runs; $i++) {
        $tmpIn = New-TemporaryFile
        [IO.File]::WriteAllText($tmpIn.FullName, "$($q.question)`nquit`n", (New-Object Text.UTF8Encoding $false))
        $null = (Get-Content $tmpIn.FullName -Raw -Encoding utf8) | & $AgentExe cli -c $Config 2>&1
        Remove-Item $tmpIn.FullName -Force
        Write-Host -NoNewline "."
    }
    Write-Host ""

    $intents = @()
    Get-ChildItem -Path $qDir -Filter "agent-trace-*.jsonl" -ErrorAction SilentlyContinue | ForEach-Object {
        Get-Content $_.FullName -Encoding utf8 | Where-Object { $_.Trim() } | ForEach-Object {
            try { $rec = $_ | ConvertFrom-Json } catch { return }
            if ($null -ne $rec.planner) { $intents += [string]$rec.planner.intent }
        }
    }
    $grouped = $intents | Group-Object | Sort-Object Count -Descending
    $distinct = ($intents | Sort-Object -Unique).Count
    $spread = ($grouped | ForEach-Object { "$($_.Name)=$($_.Count)" }) -join ", "
    $top = if ($grouped) { $grouped[0].Name } else { "<none>" }
    $matchExpect = ($top -eq $q.expect) -and ($distinct -eq 1)

    $report += [PSCustomObject]@{
        QID = $q.qid; Kind = $q.kind; Expect = $q.expect
        TopIntent = $top; Distinct = $distinct; Spread = $spread
        N = $intents.Count; MatchExpect = $matchExpect
    }
    $tag = if ($matchExpect) { "Green" } else { "Red" }
    Write-Host "    top=$top distinct=$distinct [$spread] n=$($intents.Count) match=$matchExpect" -ForegroundColor $tag
}

$summary = Join-Path $OutDir "summary_$Label.json"
$report | ConvertTo-Json -Depth 4 | Set-Content $summary -Encoding utf8
$bad = ($report | Where-Object { -not $_.MatchExpect }).Count
Write-Host ""
Write-Host "=== [$Label] $($report.Count) questions, $bad NOT matching expected+stable ===" -ForegroundColor $(if ($bad -eq 0) { "Green" } else { "Red" })
Write-Host "Summary: $summary" -ForegroundColor Gray
