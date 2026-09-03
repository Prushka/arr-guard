[CmdletBinding()]
param(
    [string]$LintVersion = 'v2.13.2'
)

$ErrorActionPreference = 'Stop'

Push-Location -LiteralPath $PSScriptRoot
try {
    & go run "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$LintVersion" run ./...
    if ($LASTEXITCODE -ne 0) {
        throw "golangci-lint exited with code $LASTEXITCODE"
    }
}
finally {
    Pop-Location
}
