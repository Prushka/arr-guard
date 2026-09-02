[CmdletBinding()]
param(
    [ValidateRange(0, 128)]
    [int]$WorkerCount = 0
)

$ErrorActionPreference = 'Stop'

$projectDirectory = $PSScriptRoot
$envPath = Join-Path $projectDirectory '.env'

if (-not (Test-Path -LiteralPath $envPath -PathType Leaf)) {
    throw "Environment file not found: $envPath"
}

foreach ($line in Get-Content -LiteralPath $envPath) {
    $trimmed = $line.Trim()
    if ($trimmed.Length -eq 0 -or $trimmed.StartsWith('#')) {
        continue
    }

    if ($trimmed -notmatch '^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$') {
        throw "Invalid .env entry: $line"
    }

    $name = $Matches[1]
    $value = $Matches[2].Trim()
    if ($value.Length -ge 2) {
        $first = $value[0]
        $last = $value[$value.Length - 1]
        if (($first -eq '"' -and $last -eq '"') -or ($first -eq "'" -and $last -eq "'")) {
            $value = $value.Substring(1, $value.Length - 2)
        }
    }

    [Environment]::SetEnvironmentVariable($name, $value, 'Process')
}

if ($WorkerCount -gt 0) {
    [Environment]::SetEnvironmentVariable('WORKERS', $WorkerCount.ToString(), 'Process')
}

Push-Location -LiteralPath $projectDirectory
try {
    & go run .
    if ($LASTEXITCODE -ne 0) {
        throw "arr-guard exited with code $LASTEXITCODE"
    }
}
finally {
    Pop-Location
}
