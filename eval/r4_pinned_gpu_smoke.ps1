# R4 live smoke: prove the deploy arm HONORS a user-named GPU (does not silently
# auto-size it away). Runs the CLI in READ-ONLY mode (mutating OFF) so it takes the
# deploy "advise" path: the matcher + selectDeployZoneAndGPU run end-to-end against
# the REAL LLM + live CompShare API, but NOTHING is created (no instance, no spend).
#
# It checks the advise reply ("推荐 GPU：<card>") for each question:
#   - pin_a100_sd : must recommend A100 (the pin) even though the SD scene-sizer
#                   would otherwise pick 4090  <-- the R4 fix
#   - control_sd  : no GPU named -> a scene card (NOT A100) -> proves the pin is
#                   what changed pin_a100_sd, not a blanket A100 bias
#   - pin_v100    : bare "V100" -> the canonical platform card "V100S"
#
# Secrets are SOURCED from the gitignored start-server.ps1 and never printed.
# Questions live in a UTF-8 JSON sidecar (this .ps1 is pure ASCII: PS 5.1 reads
# -File as ANSI). Traces go to $env:TEMP (repo path forbids Remove-Item -Recurse).
#
# Usage:  pwsh -File eval\r4_pinned_gpu_smoke.ps1
#     or  powershell -ExecutionPolicy Bypass -File eval\r4_pinned_gpu_smoke.ps1

$ErrorActionPreference = "Continue"
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$OutputEncoding = [Text.Encoding]::UTF8
[Console]::InputEncoding = [Text.Encoding]::UTF8

# Live keys from start-server.ps1 ($env: lines only; skip its build/run lines).
$startServer = "F:\compshare-agent\start-server.ps1"
if (Test-Path $startServer) {
    Get-Content $startServer | Where-Object { $_ -match '^\$env:' } | ForEach-Object { Invoke-Expression $_ }
} else {
    Write-Host "start-server.ps1 not found; set `$env:LLM_API_KEY etc. manually." -ForegroundColor Yellow
}
if (-not $env:LLM_API_KEY) {
    Write-Host "LLM_API_KEY not set - cannot run the matcher. Aborting." -ForegroundColor Red
    exit 1
}

# Read-only (advise; creates nothing), CLI path (no MySQL), trace on.
$env:COMPSHARE_ENABLE_MUTATING_TOOLS = "0"
$env:MYSQL_DSN = ""
$env:COMPSHARE_TRACE_ENABLED = "1"
if (-not $env:COMPSHARE_PROJECT_ID) { $env:COMPSHARE_PROJECT_ID = "org-cwy2qk" }

$agentExe = "F:\compshare-agent\agent.exe"
$config = "F:\compshare-agent\deploy\conf\agent.yaml"
$questionsPath = "F:\compshare-agent\eval\r4_pinned_gpu_questions.json"
$baseDir = Join-Path $env:TEMP "r4_pinned_gpu_smoke"
if (Test-Path $baseDir) { Remove-Item $baseDir -Recurse -Force }
New-Item -ItemType Directory -Force -Path $baseDir | Out-Null

$questions = [IO.File]::ReadAllText($questionsPath, [Text.Encoding]::UTF8) | ConvertFrom-Json
$report = @()

foreach ($q in $questions) {
    $qDir = Join-Path $baseDir $q.qid
    New-Item -ItemType Directory -Force -Path $qDir | Out-Null
    $env:COMPSHARE_TRACE_DIR = $qDir

    # UTF-8 WITHOUT BOM stdin: question + quit. A BOM leaks a leading char into the
    # planner input; $OutputEncoding (the pipe encoding in 5.1) must be UTF-8 too.
    $tmpIn = New-TemporaryFile
    [IO.File]::WriteAllText($tmpIn.FullName, "$($q.question)`nquit`n", (New-Object Text.UTF8Encoding $false))
    $out = (Get-Content $tmpIn.FullName -Raw -Encoding utf8) | & $agentExe cli -c $config 2>&1 | Out-String
    Remove-Item $tmpIn.FullName -Force

    # Save full stdout for inspection.
    [IO.File]::WriteAllText((Join-Path $qDir "stdout.txt"), $out, (New-Object Text.UTF8Encoding $false))

    # Extract the recommended GPU line (advise) or the deploy GPU line (if it created).
    $gpuLine = ($out -split "`n" | Where-Object { $_ -match '(推荐 GPU|GPU)[:：]' } | Select-Object -First 1)
    if ($gpuLine) { $gpuLine = $gpuLine.Trim() }

    # Planner intent from the trace (confirms it reached the deploy arm).
    $intent = ""
    Get-ChildItem -Path $qDir -Filter "agent-trace-*.jsonl" -ErrorAction SilentlyContinue | ForEach-Object {
        Get-Content $_.FullName -Encoding utf8 | Where-Object { $_.Trim() } | ForEach-Object {
            try { $rec = $_ | ConvertFrom-Json } catch { return }
            if ($null -ne $rec.intent_router -and $rec.intent_router.intent) { $intent = [string]$rec.intent_router.intent }
        }
    }

    # Did the advise reply mention the expected card?
    $hitA100 = $out -match 'A100'
    $hitV100S = $out -match 'V100S'
    $verdict = switch ($q.qid) {
        "pin_a100_sd" { if ($hitA100) { "PASS" } else { "FAIL" } }
        "control_sd"  { if ($hitA100) { "CHECK(A100 present w/o pin)" } else { "PASS" } }
        "pin_v100"    { if ($hitV100S) { "PASS" } else { "FAIL" } }
        default       { "?" }
    }

    $report += [PSCustomObject]@{ QID = $q.qid; Intent = $intent; GpuLine = $gpuLine; Verdict = $verdict }

    Write-Host ""
    Write-Host ">>> [$($q.qid)] intent=$intent  verdict=$verdict" -ForegroundColor Cyan
    Write-Host "    Q: $($q.question)" -ForegroundColor Gray
    Write-Host "    GPU line: $gpuLine" -ForegroundColor Gray
}

Write-Host ""
Write-Host "=== R4 pinned-GPU smoke ===" -ForegroundColor Yellow
$report | Format-Table -AutoSize
Write-Host "Full stdout per question under: $baseDir" -ForegroundColor Gray
