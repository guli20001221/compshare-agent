# check_skill_caps.ps1 — pre-commit gate (ADR-004 §150-151).
# Asserts every internal/skills/<dir>/skill.md body is within its line cap. The
# cap is body_cap_lines (default 100, hard ceiling 200); frontmatter lines are
# NOT counted. Build/load fails on over-cap (never silent truncation). The
# authoritative gate is `go test ./internal/skills` (Skill.Body); this mirrors
# its counting rule (trailing blank lines ignored) as the fast local guard.

$ErrorActionPreference = "Stop"
$root = (git rev-parse --show-toplevel).Trim()
$skillsDir = Join-Path $root "internal/skills"

$DefaultCap = 100
$MaxCap = 200

function Fail($message) {
    Write-Error $message
    exit 1
}

if (-not (Test-Path -LiteralPath $skillsDir)) {
    exit 0
}

# countLines mirrors internal/skills.countLines: trim trailing newlines, then
# count remaining lines (0 for an empty body).
function Get-BodyLineCount($body) {
    $trimmed = $body.TrimEnd("`n")
    if ($trimmed -eq "") { return 0 }
    return ($trimmed -split "`n").Count
}

$violations = 0
foreach ($dir in Get-ChildItem -LiteralPath $skillsDir -Directory) {
    $skillFile = Join-Path $dir.FullName "skill.md"
    if (-not (Test-Path -LiteralPath $skillFile)) {
        continue
    }
    $raw = Get-Content -Raw -LiteralPath $skillFile
    $lf = ($raw -replace "`r`n", "`n") -replace "`r", "`n"
    if (-not $lf.StartsWith("---`n")) {
        Write-Host "FAIL $($dir.Name): missing frontmatter opener"
        $violations++
        continue
    }
    $afterOpen = $lf.Substring(4)
    $closerIdx = $afterOpen.IndexOf("`n---")
    if ($closerIdx -lt 0) {
        Write-Host "FAIL $($dir.Name): missing frontmatter closer"
        $violations++
        continue
    }
    $frontmatter = $afterOpen.Substring(0, $closerIdx)
    $body = $afterOpen.Substring($closerIdx + 4) -replace '^[\r\n]+', ''

    $cap = $DefaultCap
    $capMatch = [regex]::Match($frontmatter, '(?m)^body_cap_lines:\s*(\d+)\s*$')
    if ($capMatch.Success) {
        $declared = [int]$capMatch.Groups[1].Value
        if ($declared -gt $MaxCap) {
            Write-Host "FAIL $($dir.Name): body_cap_lines $declared exceeds hard ceiling $MaxCap"
            $violations++
            continue
        }
        if ($declared -gt 0) { $cap = $declared }
    }

    $count = Get-BodyLineCount $body
    if ($count -gt $cap) {
        Write-Host "FAIL $($dir.Name): body has $count lines, exceeds cap $cap"
        $violations++
    }
}

if ($violations -gt 0) {
    Fail "check_skill_caps: $violations skill body-cap violation(s)"
}
exit 0
