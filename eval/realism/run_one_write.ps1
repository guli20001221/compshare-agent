# Single write-op scenario driver (mutating-on). Drives one CLI session with a
# scripted multi-line stdin (request + confirm answers + quit), then dumps the
# full reply + trace summary (intent / hard_block / tool calls / confirm markers).
#
# Used to exercise the confirm-gated mutating workflows against a TEST account
# with explicit owner authorization (mutating-on, create allowed, no cleanup).
# Sources creds from gitignored start-server.ps1; never prints secrets.
#
# Usage:
#   powershell -File eval\realism\run_one_write.ps1 -Label create_4090 -Lines @("创建一个4090实例,按量计费,用pytorch镜像","y","quit")
#   powershell -File eval\realism\run_one_write.ps1 -Label gate_decline -Lines @("帮我关机 <id>","n","quit")
#
# cwd MUST be the worktree with deploy/kb (corpus). MUTATING is ON here.

param(
    [Parameter(Mandatory=$true)][string]$Label,
    [Parameter(Mandatory=$true)][string[]]$Lines,
    [string]$Org = "org-cwy2qk",
    [string]$OutDir = "eval\realism\out_write"
)

$ErrorActionPreference = "Continue"
[Console]::OutputEncoding = [Text.Encoding]::UTF8
[Console]::InputEncoding = [Text.Encoding]::UTF8
$OutputEncoding = [Text.Encoding]::UTF8

$startServer = "F:\compshare-agent\start-server.ps1"
Get-Content $startServer | Where-Object { $_ -match '^\$env:' } | ForEach-Object { Invoke-Expression $_ }
if (-not $env:LLM_API_KEY) { Write-Host "LLM_API_KEY not set. Aborting." -ForegroundColor Red; exit 1 }

# WRITE leg: mutating ON (exactly "1"; other values are treated as off).
$env:COMPSHARE_ENABLE_MUTATING_TOOLS = "1"
$env:MYSQL_DSN = ""
$env:COMPSHARE_TRACE_ENABLED = "1"
$env:COMPSHARE_PROJECT_ID = $Org

$agentExe = Join-Path (Get-Location) "agent.exe"
$baseConfig = "F:\compshare-agent\deploy\conf\agent.yaml"
$root = Join-Path (Get-Location) $OutDir
New-Item -ItemType Directory -Force -Path $root | Out-Null
$orgConfig = Join-Path $root "agent.org.yaml"
$orig = [IO.File]::ReadAllText($baseConfig, [Text.Encoding]::UTF8)
[IO.File]::WriteAllText($orgConfig, ($orig -replace 'project_id:\s*""', 'project_id: "${COMPSHARE_PROJECT_ID}"'), (New-Object Text.UTF8Encoding $false))

$qDir = Join-Path $root "traces\$Label"
if (Test-Path $qDir) { Remove-Item $qDir -Recurse -Force }
New-Item -ItemType Directory -Force -Path $qDir | Out-Null
$env:COMPSHARE_TRACE_DIR = $qDir

$stdin = ($Lines -join "`n") + "`n"
$tmpIn = New-TemporaryFile
[IO.File]::WriteAllText($tmpIn.FullName, $stdin, (New-Object Text.UTF8Encoding $false))
Write-Host "=== [$Label] mutating=ON  stdin lines: $($Lines.Count) ===" -ForegroundColor Magenta
$out = (Get-Content $tmpIn.FullName -Raw -Encoding utf8) | & $agentExe cli -c $orgConfig 2>&1 | Out-String
Remove-Item $tmpIn.FullName -Force
[IO.File]::WriteAllText((Join-Path $root "$Label.txt"), $out, (New-Object Text.UTF8Encoding $false))

Write-Host $out
Write-Host "--- trace summary ---" -ForegroundColor Cyan
Get-ChildItem -Path $qDir -Filter "agent-trace-*.jsonl" -ErrorAction SilentlyContinue | ForEach-Object {
    Get-Content $_.FullName -Encoding utf8 | Where-Object { $_.Trim() } | ForEach-Object {
        try { $rec = $_ | ConvertFrom-Json } catch { return }
        $intent = if ($rec.planner) { $rec.planner.intent } else { "" }
        $hb = if ($rec.engine_hard_block -and $rec.engine_hard_block.hit) { "HB:" + $rec.engine_hard_block.category } else { "" }
        $tnames = @(); foreach ($tc in @($rec.tool_calls)) { if ($tc.name) { $tnames += "$($tc.name)(ok=$($tc.ok)$($tc.error)$($tc.err))" } elseif ($tc.action) { $tnames += $tc.action } }
        if ($intent -or $hb -or $tnames.Count) { Write-Host ("turn: intent={0} {1} tools=[{2}]" -f $intent, $hb, ($tnames -join ", ")) -ForegroundColor DarkGray }
    }
}
Write-Host "Saved: $(Join-Path $root "$Label.txt")  trace: $qDir" -ForegroundColor Gray
