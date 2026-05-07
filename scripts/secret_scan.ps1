param(
    [switch]$Staged
)

$ErrorActionPreference = "Stop"

$root = (git rev-parse --show-toplevel).Trim()
Set-Location $root

$forbiddenPathPatterns = @(
    '^deploy/conf/agent\.yaml$',
    '^eval/shadow_qa/agent\.ya?ml$',
    '^eval/shadow_qa/.*/agent\.ya?ml$',
    '^eval/shadow_qa/shadow_qa_agent\.ya?ml$',
    '^eval/shadow_qa/.*/shadow_qa_agent\.ya?ml$',
    '^.*\.env$'
)

$secretPatterns = @(
    'api_key:\s*["''][^"$][^"'']{16,}["'']',
    'public_key:\s*["''][^"$][^"'']{16,}["'']',
    'private_key:\s*["''][^"$][^"'']{16,}["'']',
    '(?i)(access|secret|api|auth|session|jupyter|bearer|compshare|mverse|modelverse|ucloud|ark|volc|llm|hf).{0,24}(key|token)\s*[:=]\s*["'']?[A-Za-z0-9_\-]{24,}'
)

function Fail($message) {
    Write-Error $message
    exit 1
}

if ($Staged) {
    $paths = git -c core.quotepath=false diff --cached --name-only --diff-filter=ACMR
    foreach ($path in $paths) {
        foreach ($pattern in $forbiddenPathPatterns) {
            if ($path -match $pattern -and $path -notmatch '\.example$') {
                Fail "Refusing to commit secret-bearing local file: $path"
            }
        }
    }

    $diff = git diff --cached --unified=0
    foreach ($pattern in $secretPatterns) {
        if ($diff -match $pattern) {
            Fail "Potential secret detected in staged diff. Replace literals with environment placeholders."
        }
    }
    exit 0
}

$trackedAndUntracked = git -c core.quotepath=false ls-files --cached --others --exclude-standard
foreach ($path in $trackedAndUntracked) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf -ErrorAction SilentlyContinue)) {
        continue
    }
    foreach ($pattern in $forbiddenPathPatterns) {
        if ($path -match $pattern -and $path -notmatch '\.example$') {
            Fail "Secret-bearing local config is present in git-visible files: $path"
        }
    }
    $content = Get-Content -LiteralPath $path -Raw -ErrorAction SilentlyContinue
    if ($null -eq $content) {
        continue
    }
    foreach ($pattern in $secretPatterns) {
        if ($content -match $pattern) {
            Fail "Potential secret detected in $path. Replace literals with environment placeholders."
        }
    }
}
