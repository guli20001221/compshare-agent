param(
    [switch]$Staged
)

$ErrorActionPreference = "Stop"

$root = (git rev-parse --show-toplevel).Trim()
Set-Location $root

$forbiddenPathPatterns = @(
    '^deploy/conf/agent\.yaml$',
    '^.*\.env$'
)

$secretPatterns = @(
    'api_key:\s*["''][^"$][^"'']{16,}["'']',
    'public_key:\s*["''][^"$][^"'']{16,}["'']',
    'private_key:\s*["''][^"$][^"'']{16,}["'']',
    '(?i)(access|secret|api|auth|session|jupyter|bearer|compshare|mverse|modelverse|ucloud|ark|volc|llm|hf).{0,24}(key|token)\s*[:=]\s*["'']?[A-Za-z0-9_\-]{24,}',
    '(?i)(password|client_secret|webhook_secret|credential)\s*[:=]\s*["'']?[A-Za-z0-9_\-/+=.]{8,}',
    '(?i)\bbearer\s+[A-Za-z0-9_\-.]{20,}'
)

$allowedSecretFiles = @(
    'deploy/conf/config.local.yaml'
)

function IsAllowedSecretFile($path) {
    foreach ($allowed in $allowedSecretFiles) {
        if ($path -eq $allowed) {
            return $true
        }
    }
    return $false
}

function IsGeneratedNonSecretFile($path) {
    # Embedding sidecars contain only chunk IDs and numeric vectors; image assets
    # are binary. Reading either as text makes the release scan unnecessarily slow.
    if ($path -match '^deploy/kb/embeddings_[^/]+\.jsonl$') {
        return $true
    }
    # RAG V2 corpora and model locks intentionally preserve pinned public API
    # examples without redaction. Source selection, not generic regex rewriting,
    # is their trust boundary; code/config files remain covered by this scan.
    if ($path -match '^deploy/kb/(stage2b_w0|external_w0)\.jsonl$') {
        return $true
    }
    if ($path -match '^deploy/kb/v2/(stage2b_v2|external_v2|legacy_external_lock)\.jsonl$') {
        return $true
    }
    if ($path -match '^deploy/kb/v2/(asset_lock|asset_report)\.json$') {
        return $true
    }
    if ($path -match '^deploy/kb/v2/assets/.*\.(png|jpe?g|gif|webp|bmp|tiff?)$') {
        return $true
    }
    return $false
}

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

    foreach ($path in $paths) {
        if ((IsAllowedSecretFile $path) -or (IsGeneratedNonSecretFile $path)) {
            continue
        }
        # Scan only newly added content. Removed lines may contain the exact
        # secret-shaped text this change is deleting and must not make cleanup
        # commits impossible. Diff headers are excluded explicitly.
        $diff = git diff --cached --unified=0 -- $path |
            Where-Object { $_ -match '^\+' -and $_ -notmatch '^\+\+\+' }
        foreach ($pattern in $secretPatterns) {
            if ($diff -match $pattern) {
                Fail "Potential secret detected in staged diff. Replace literals with environment placeholders."
            }
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
    if ((IsAllowedSecretFile $path) -or (IsGeneratedNonSecretFile $path)) {
        continue
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
