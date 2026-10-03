#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$BinaryPath,
    [Parameter(Mandatory)][string]$Tag,
    [Parameter(Mandatory)][ValidatePattern('^[0-9a-f]{40}$')][string]$SourceCommit,
    [Parameter(Mandatory)][string]$BuildDate,
    [switch]$RequireSignature,
    [string]$CertificateThumbprint,
    [string]$ManifestPath,
    [string]$RequestID,
    [string]$SigningRunID,
    [string]$SigningRunAttempt,
    [string]$ChecksumsPath
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$binary = (Resolve-Path -LiteralPath $BinaryPath).Path
if ($RequireSignature) {
    if ($CertificateThumbprint -cnotmatch '^[0-9A-Fa-f]{40}$') {
        throw 'missing or invalid RUNTIME_SIGNING_CERTIFICATE_THUMBPRINT'
    }
    $signature = Get-AuthenticodeSignature -LiteralPath $binary
    if ($signature.Status -ne 'Valid' -or $null -eq $signature.SignerCertificate -or
        $signature.SignerCertificate.Thumbprint -ine $CertificateThumbprint -or
        $null -eq $signature.TimeStamperCertificate) {
        throw 'runtime Authenticode signature or timestamp is invalid'
    }
}
$hash = (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash.ToLowerInvariant()
if ($ManifestPath) {
    $manifestJSON = Get-Content -LiteralPath $ManifestPath -Raw
    $manifest = $manifestJSON | ConvertFrom-Json -Depth 8
    $document = [System.Text.Json.JsonDocument]::Parse($manifestJSON)
    try { $manifest.buildDate = $document.RootElement.GetProperty('buildDate').GetString() }
    finally { $document.Dispose() }
    if ($manifest.schema -ne 1 -or $manifest.tag -cne $Tag -or $manifest.commit -cne $SourceCommit -or
        $manifest.buildDate -cne $BuildDate -or $manifest.requestID -cne $RequestID -or
        [string]$manifest.signingRunID -cne $SigningRunID -or
        [string]$manifest.signingRunAttempt -cne $SigningRunAttempt -or
        $manifest.binaryName -cne (Split-Path -Leaf $binary) -or $manifest.sha256 -cne $hash) {
        throw 'runtime signing manifest does not match the release request'
    }
}
if ($ChecksumsPath) {
    $line = (Get-Content -LiteralPath $ChecksumsPath -Raw).TrimEnd("`r", "`n")
    if ($line -cne "$hash  $(Split-Path -Leaf $binary)") { throw 'runtime release checksum mismatch' }
}
$metadata = [Diagnostics.FileVersionInfo]::GetVersionInfo($binary)
if ($metadata.ProductName -cne 'AUTO-MAS Runtime' -or $metadata.ProductVersion -cne $Tag -or
    $metadata.FileVersion -cne $Tag -or $metadata.OriginalFilename -cne 'auto-mas-runtime.exe') {
    throw 'runtime Windows version metadata does not match the release'
}
# 验签与哈希检查先于执行，遥测在执行前禁用。
$env:AUTO_MAS_TELEMETRY = 'disabled'
$lines = @(& $binary version --output ndjson)
if ($LASTEXITCODE -ne 0) { throw 'runtime version command failed' }
$events = @(
    foreach ($line in $lines) {
        $document = [System.Text.Json.JsonDocument]::Parse([string]$line)
        try {
            if ($document.RootElement.ValueKind -ne [System.Text.Json.JsonValueKind]::Object) {
                throw 'runtime version emitted a non-object NDJSON value'
            }
            $event = $line | ConvertFrom-Json -ErrorAction Stop -NoEnumerate
            if ($event.type -ceq 'result') {
                # 保留 JSON 字符串，避免 pwsh 自动把 ISO 时间变成 DateTime 后丢失字面身份。
                $event.details.buildDate = $document.RootElement.GetProperty('details').GetProperty('buildDate').GetString()
            }
            $event
        }
        finally { $document.Dispose() }
    }
)
$results = @($events | Where-Object { $_.type -ceq 'result' })
if ($events.Count -lt 2 -or $events[0].type -cne 'hello' -or
    $events[0].runtimeVersion -cne $Tag -or $results.Count -ne 1 -or
    $events[-1].type -cne 'result' -or $results[0].success -ne $true -or
    $results[0].details.runtimeVersion -cne $Tag -or $results[0].details.commit -cne $SourceCommit -or
    $results[0].details.buildDate -cne $BuildDate) {
    throw 'runtime version identity does not match the release'
}
Write-Host "Runtime release verified: $Tag $SourceCommit"
