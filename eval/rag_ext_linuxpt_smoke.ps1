# Linux-ops + PyTorch-basics external-corpus CLI smoke. Authoritative faithfulness
# check per memory feedback-cli-eval-flash-only: real engine + merged index
# (platform + external, default-on) + deepseek-v4-flash, NOT the offline v4-pro
# judge (which over-reports fab). For each question it pipes the CJK prompt to
# `agent.exe cli` once, captures the reply text (UTF-8) and the per-question
# JSONL trace, so the companion judge (rag_ext_linuxpt_smoke_judge.py) can assert
# the expected external chunk was retrieved/cited AND a grounded anchor token
# appears in the reply. The ssh-免密 row is the merged-index collision check: the
# query infers product_area=login (the SSH-key chunk gets no +2 affinity boost),
# yet it must still retrieve + cite the external ext-linux-ssh-key-001 chunk.
#
# Run from the WORKTREE root so the relative deploy/kb paths resolve to THIS
# branch's 51-chunk corpus + new sidecar (not main's). Read-only (mutating off);
# the only creds needed are the LLM/embedding key. Never prints secrets.
#
# Usage:  pwsh -File eval\rag_ext_linuxpt_smoke.ps1
param(
    [string]$WorktreeRoot = "F:\compshare-agent-worktrees\p4-linux-pytorch"
)
$ErrorActionPreference = "Continue"
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$OutputEncoding = [Text.Encoding]::UTF8
[Console]::InputEncoding = [Text.Encoding]::UTF8

Set-Location $WorktreeRoot

# Source the purpose-built smoke env first (STS service keys + role urn +
# LLM_API_KEY as $env: lines), then .env.local for MODELVERSE_API_KEY (qwen3
# retrieval) + legacy AK/SK read-only fallback. Never echoes the values.
$smokeEnv = Join-Path $WorktreeRoot "eval\.smoke_env.ps1"
if (Test-Path $smokeEnv) {
    Get-Content $smokeEnv | Where-Object { $_ -match '^\s*\$env:' } | ForEach-Object { Invoke-Expression $_ }
}
# Load KEY=VALUE pairs from .env.local into the environment (agent.yaml does
# ${ENV_VAR} substitution). Never echoes the values.
$envLocal = Join-Path $WorktreeRoot ".env.local"
if (Test-Path $envLocal) {
    foreach ($line in Get-Content $envLocal) {
        $t = $line.Trim()
        if ($t -and -not $t.StartsWith("#") -and $t.Contains("=")) {
            $k = $t.Substring(0, $t.IndexOf("=")).Trim()
            $v = $t.Substring($t.IndexOf("=") + 1).Trim()
            if ($k) { Set-Item -Path "Env:$k" -Value $v }
        }
    }
}
if (-not $env:LLM_API_KEY) {
    Write-Host "LLM_API_KEY not set (need it in .env.local). Aborting." -ForegroundColor Red
    exit 1
}

# Read-only; no MySQL; trace on. External + agentic SearchKnowledge are DEFAULT-ON
# (resolved in cmd, #242) so we deliberately do NOT set them — testing the real
# default path on the merged index.
$env:COMPSHARE_ENABLE_MUTATING_TOOLS = "0"
$env:MYSQL_DSN = ""
$env:COMPSHARE_TRACE_ENABLED = "1"
# Config load requires a non-empty project_id; RAG/knowledge turns never call a
# CompShare API so the value is immaterial — use the read-only live project.
if (-not $env:COMPSHARE_PROJECT_ID) { $env:COMPSHARE_PROJECT_ID = "org-cwy2qk" }

$agentExe = Join-Path $WorktreeRoot "agent.exe"
# Reproducible from a CLEAN checkout: deploy/conf/agent.yaml is gitignored (a dev
# fills it locally), but the committed deploy/conf/agent.yaml.example carries the
# same ${ENV_VAR} placeholders, resolved from the env loaded above. Prefer a local
# agent.yaml if present, else fall back to the committed example; fail loud if
# neither exists. (Passing -c explicitly bypasses the CLI's own example fallback.)
$config = Join-Path $WorktreeRoot "deploy\conf\agent.yaml"
if (-not (Test-Path $config)) { $config = Join-Path $WorktreeRoot "deploy\conf\agent.yaml.example" }
$baseDir = Join-Path $WorktreeRoot "eval\traces_linuxpt_smoke"
$questionsPath = Join-Path $WorktreeRoot "eval\rag_ext_linuxpt_smoke_questions.json"

if (-not (Test-Path $agentExe)) {
    Write-Host "agent.exe not found - build first: go build -o agent.exe ./cmd" -ForegroundColor Red
    exit 1
}
if (-not (Test-Path $config)) {
    Write-Host "no agent.yaml or agent.yaml.example under deploy/conf - aborting." -ForegroundColor Red
    exit 1
}
New-Item -ItemType Directory -Force -Path $baseDir | Out-Null

$questions = [IO.File]::ReadAllText($questionsPath, [Text.Encoding]::UTF8) | ConvertFrom-Json

foreach ($q in $questions) {
    $qDir = Join-Path $baseDir $q.qid
    # Wipe any prior run so the dated trace file holds ONLY this run (it appends).
    # .NET recursive delete is NonInteractive-safe (Remove-Item -Recurse prompts
    # under -NonInteractive); the dir is a gitignored scratch trace dir.
    if (Test-Path $qDir) { [IO.Directory]::Delete((Convert-Path $qDir), $true) }
    New-Item -ItemType Directory -Force -Path $qDir | Out-Null
    $env:COMPSHARE_TRACE_DIR = $qDir

    Write-Host ""
    Write-Host ">>> [$($q.qid)] $($q.kind)" -ForegroundColor Cyan
    Write-Host "    Q: $($q.question)" -ForegroundColor Gray

    $inputText = "$($q.question)`nquit`n"
    $tmpIn = New-TemporaryFile
    [IO.File]::WriteAllText($tmpIn.FullName, $inputText, (New-Object Text.UTF8Encoding $false))
    $reply = (Get-Content $tmpIn.FullName -Raw -Encoding utf8) | & $agentExe cli -c $config 2>&1 | Out-String
    Remove-Item $tmpIn.FullName -Force

    # Persist the reply (UTF-8 no BOM) for the Python judge.
    $replyPath = Join-Path $qDir "reply.txt"
    [IO.File]::WriteAllText($replyPath, $reply, (New-Object Text.UTF8Encoding $false))
    Write-Host "    reply saved ($($reply.Length) chars) + trace in $qDir" -ForegroundColor DarkGray
}

Write-Host ""
Write-Host "Done. Judge with: python eval\rag_ext_linuxpt_smoke_judge.py" -ForegroundColor Green
