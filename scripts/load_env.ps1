param(
    [string]$Path = ".env.local"
)

$ErrorActionPreference = "Stop"

if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
    Write-Error "env file not found: $Path"
    exit 1
}

$loaded = New-Object System.Collections.Generic.List[string]
$lineNo = 0
foreach ($line in Get-Content -LiteralPath $Path) {
    $lineNo++
    $trimmed = $line.Trim()
    if ($trimmed -eq "" -or $trimmed.StartsWith("#")) {
        continue
    }
    if ($trimmed.StartsWith("export ")) {
        $trimmed = $trimmed.Substring(7).Trim()
    }

    $idx = $trimmed.IndexOf("=")
    if ($idx -le 0) {
        Write-Error "invalid env syntax at ${Path}:${lineNo}; expected KEY=VALUE"
        exit 1
    }

    $key = $trimmed.Substring(0, $idx).Trim()
    $value = $trimmed.Substring($idx + 1).Trim()
    if ($key -notmatch '^[A-Za-z_][A-Za-z0-9_]*$') {
        Write-Error "invalid environment variable name at ${Path}:${lineNo}: $key"
        exit 1
    }

    if (($value.StartsWith('"') -and $value.EndsWith('"')) -or ($value.StartsWith("'") -and $value.EndsWith("'"))) {
        $value = $value.Substring(1, $value.Length - 2)
    }

    [Environment]::SetEnvironmentVariable($key, $value, "Process")
    $loaded.Add($key) | Out-Null
}

if ($loaded.Count -eq 0) {
    Write-Warning "no environment variables loaded from $Path"
} else {
    Write-Output ("loaded env vars: " + ($loaded -join ", "))
}
