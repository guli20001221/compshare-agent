# Realism eval runner — drives paraphrased real after-sales questions (聊天记录.md)
# through the shipped CLI on main, one FRESH session per question (clean routing,
# no PriorText bleed). Read-only by default. Captures reply + trace intent +
# tool calls + citation markers for judging.
#
# Input realism source: F:\compshare-agent\聊天记录.md (WeCom after-sales group).
# The log is used ONLY as test questions + a gap-map, never as corpus content.
#
# Usage:
#   powershell -File eval\realism\run_realism_eval.ps1 [-N 1] [-Org "org-cwy2qk"] [-Questions <jsonl>]
#
# Must run with cwd = the worktree that has deploy/kb/ (platform + external corpus),
# so retrieval matches shipped behavior. Sources creds from start-server.ps1
# (gitignored). Never prints/commits secrets.

param(
    [int]$N = 1,
    [string]$Org = "org-cwy2qk",
    [string]$Questions = "eval\realism\realism_questions.jsonl",
    [string]$OutDir = "eval\realism\out"
)

$ErrorActionPreference = "Continue"
[Console]::OutputEncoding = [Text.Encoding]::UTF8
[Console]::InputEncoding = [Text.Encoding]::UTF8
$OutputEncoding = [Text.Encoding]::UTF8

# --- creds (sourced from gitignored start-server.ps1; values never printed) ---
$startServer = "F:\compshare-agent\start-server.ps1"
if (Test-Path $startServer) {
    Get-Content $startServer | Where-Object { $_ -match '^\$env:' } | ForEach-Object { Invoke-Expression $_ }
} else {
    Write-Host "start-server.ps1 not found; set creds manually." -ForegroundColor Yellow; exit 1
}
if (-not $env:LLM_API_KEY) { Write-Host "LLM_API_KEY not set. Aborting." -ForegroundColor Red; exit 1 }

# Read-only for the realism pass. ("0" is treated as unknown->off by the agent;
# we unset to be unambiguous.)
Remove-Item Env:\COMPSHARE_ENABLE_MUTATING_TOOLS -ErrorAction SilentlyContinue
$env:MYSQL_DSN = ""
$env:COMPSHARE_TRACE_ENABLED = "1"
$env:COMPSHARE_PROJECT_ID = $Org

$agentExe = Join-Path (Get-Location) "agent.exe"
if (-not (Test-Path $agentExe)) { Write-Host "agent.exe missing in cwd $(Get-Location); build from ./cmd first." -ForegroundColor Red; exit 1 }

# Org config: real agent.yaml with project_id pinned to ${COMPSHARE_PROJECT_ID}.
$baseConfig = "F:\compshare-agent\deploy\conf\agent.yaml"
$root = Join-Path (Get-Location) $OutDir
New-Item -ItemType Directory -Force -Path $root | Out-Null
$orgConfig = Join-Path $root "agent.org.yaml"
$orig = [IO.File]::ReadAllText($baseConfig, [Text.Encoding]::UTF8)
[IO.File]::WriteAllText($orgConfig, ($orig -replace 'project_id:\s*""', 'project_id: "${COMPSHARE_PROJECT_ID}"'), (New-Object Text.UTF8Encoding $false))

$replyDir = Join-Path $root "replies"; New-Item -ItemType Directory -Force -Path $replyDir | Out-Null
$results = @()

$qs = Get-Content $Questions -Encoding utf8 | Where-Object { $_.Trim() } | ForEach-Object { $_ | ConvertFrom-Json }
Write-Host "Loaded $($qs.Count) questions; N=$N runs each; org=$Org" -ForegroundColor Magenta

foreach ($item in $qs) {
    for ($run = 1; $run -le $N; $run++) {
        $tag = if ($N -eq 1) { $item.id } else { "$($item.id)_r$run" }
        $qDir = Join-Path $root "traces\$tag"
        if (Test-Path $qDir) { Remove-Item $qDir -Recurse -Force }
        New-Item -ItemType Directory -Force -Path $qDir | Out-Null
        $env:COMPSHARE_TRACE_DIR = $qDir

        $tmpIn = New-TemporaryFile
        [IO.File]::WriteAllText($tmpIn.FullName, "$($item.q)`nquit`n", (New-Object Text.UTF8Encoding $false))
        $out = (Get-Content $tmpIn.FullName -Raw -Encoding utf8) | & $agentExe cli -c $orgConfig 2>&1 | Out-String
        Remove-Item $tmpIn.FullName -Force
        [IO.File]::WriteAllText((Join-Path $replyDir "$tag.txt"), $out, (New-Object Text.UTF8Encoding $false))

        # Extract assistant reply (between 'Assistant>' and the next 'You>'/end).
        $reply = ""
        if ($out -match '(?s)Assistant>\s*(.*?)\s*(?:\r?\nYou>|\Z)') { $reply = $Matches[1].Trim() }
        $replyOne = ($reply -replace '\s+', ' ')
        if ($replyOne.Length -gt 160) { $replyOne = $replyOne.Substring(0,160) + "..." }

        # Trace: intent + tool calls.
        $intent = ""; $tools = @()
        Get-ChildItem -Path $qDir -Filter "agent-trace-*.jsonl" -ErrorAction SilentlyContinue | ForEach-Object {
            Get-Content $_.FullName -Encoding utf8 | Where-Object { $_.Trim() } | ForEach-Object {
                try { $rec = $_ | ConvertFrom-Json } catch { return }
                if ($rec.planner -and $rec.planner.intent) { $intent = $rec.planner.intent }
                # Keyword preblocks short-circuit BEFORE the planner (no planner record).
                # Record them as HB:<category> so routing is fully visible.
                if ($rec.engine_hard_block -and $rec.engine_hard_block.hit) { $intent = "HB:" + $rec.engine_hard_block.category }
                foreach ($tc in @($rec.tool_calls)) { if ($tc.name) { $tools += $tc.name } elseif ($tc.action) { $tools += $tc.action } }
            }
        }
        $toolsStr = ($tools | Select-Object -Unique) -join ","
        $cited = if ($reply -match '【|】|\[\[|\]\]|\[[0-9]+\]') { 1 } else { 0 }
        $searchKB = if ($toolsStr -match 'SearchKnowledge') { 1 } else { 0 }

        $results += [pscustomobject]@{ id=$tag; intent=$intent; sk=$searchKB; cited=$cited; tools=$toolsStr; reply=$replyOne }
        Write-Host ("[{0}] intent={1} sk={2} cited={3} :: {4}" -f $tag, $intent, $searchKB, $cited, $replyOne) -ForegroundColor Gray
    }
}

$tsv = Join-Path $root "results.tsv"
$results | Export-Csv -Path $tsv -Delimiter "`t" -NoTypeInformation -Encoding utf8
Write-Host ""
Write-Host "Results: $tsv" -ForegroundColor Green
Write-Host "Replies: $replyDir" -ForegroundColor Green
Write-Host "=== intent distribution ===" -ForegroundColor Cyan
$results | Group-Object intent | Sort-Object Count -Descending | ForEach-Object { Write-Host ("  {0,-22} {1}" -f $_.Name, $_.Count) }
