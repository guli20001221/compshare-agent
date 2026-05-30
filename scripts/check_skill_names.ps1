# check_skill_names.ps1 — pre-commit gate (ADR-004 §69).
# Asserts every internal/skills/<dir>/skill.md frontmatter `name` equals its
# directory name and matches the snake_case pattern [a-z][a-z0-9_]*[a-z0-9] (1-64
# chars). The authoritative gate is `go test ./internal/skills` (ParseSkillFile);
# this is the fast local guard.

$ErrorActionPreference = "Stop"
$root = (git rev-parse --show-toplevel).Trim()
$skillsDir = Join-Path $root "internal/skills"

function Fail($message) {
    Write-Error $message
    exit 1
}

if (-not (Test-Path -LiteralPath $skillsDir)) {
    exit 0
}

$violations = 0
foreach ($dir in Get-ChildItem -LiteralPath $skillsDir -Directory) {
    $skillFile = Join-Path $dir.FullName "skill.md"
    if (-not (Test-Path -LiteralPath $skillFile)) {
        continue
    }
    $raw = Get-Content -Raw -LiteralPath $skillFile
    $lf = ($raw -replace "`r`n", "`n") -replace "`r", "`n"
    $nameMatch = [regex]::Match($lf, '(?m)^name:\s*(.+?)\s*$')
    if (-not $nameMatch.Success) {
        Write-Host "FAIL $($dir.Name): no `name:` field in skill.md"
        $violations++
        continue
    }
    $name = $nameMatch.Groups[1].Value.Trim()
    if ($name -ne $dir.Name) {
        Write-Host "FAIL $($dir.Name): name `"$name`" != directory name"
        $violations++
    }
    if ($name.Length -gt 64 -or ($name -notmatch '^[a-z][a-z0-9_]*[a-z0-9]$')) {
        Write-Host "FAIL $($dir.Name): name `"$name`" is not snake_case [a-z][a-z0-9_]*[a-z0-9] (1-64)"
        $violations++
    }
}

if ($violations -gt 0) {
    Fail "check_skill_names: $violations skill name violation(s)"
}
exit 0
