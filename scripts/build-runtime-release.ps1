#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidatePattern('^v\d+\.\d+\.\d+(?:[-+][A-Za-z0-9._-]+)?$')][string]$Tag,
    [Parameter(Mandatory)][ValidatePattern('^[0-9a-f]{40}$')][string]$SourceCommit,
    [Parameter(Mandatory)][ValidatePattern('^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$')][string]$BuildDate,
    [Parameter(Mandatory)][string]$OutputDirectory
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$resource = Join-Path $root 'cmd/auto-mas-runtime/resource_windows_amd64.syso'
$config = Join-Path $OutputDirectory 'versioninfo.json'
$binary = Join-Path $OutputDirectory 'auto-mas-runtime.exe'
New-Item -ItemType Directory -Path $OutputDirectory -Force | Out-Null
$match = [regex]::Match($Tag, '^v(\d+)\.(\d+)\.(\d+)')
$parts = @(1..3 | ForEach-Object { [int]$match.Groups[$_].Value })
if (@($parts | Where-Object { $_ -gt 65535 }).Count -ne 0) { throw 'runtime numeric version exceeds Windows resource limits' }
$version = @{ Major = $parts[0]; Minor = $parts[1]; Patch = $parts[2]; Build = 0 }
@{
    FixedFileInfo = @{ FileVersion = $version; ProductVersion = $version; FileFlagsMask = '3f'; FileFlags = '00'; FileOS = '040004'; FileType = '01'; FileSubType = '00' }
    StringFileInfo = @{
        CompanyName = 'AUTO-MAS Project'; FileDescription = 'AUTO-MAS Runtime'
        ProductName = 'AUTO-MAS Runtime'; FileVersion = $Tag; ProductVersion = $Tag
        InternalName = 'auto-mas-runtime'; OriginalFilename = 'auto-mas-runtime.exe'
        LegalCopyright = 'AUTO-MAS Project'
    }
    VarFileInfo = @{ Translation = @{ LangID = '0409'; CharsetID = '04B0' } }
} | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $config -Encoding utf8NoBOM
if (Test-Path -LiteralPath $resource) { throw 'runtime version resource already exists' }
Push-Location $root
try {
    & go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.7.0 -64 -o $resource $config
    if ($LASTEXITCODE -ne 0) { throw 'runtime version resource generation failed' }
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $ldflags = @('-s', '-w',
        "-X github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version.Version=$Tag",
        "-X github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version.Commit=$SourceCommit",
        "-X github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version.BuildDate=$BuildDate")
    if (-not [string]::IsNullOrWhiteSpace($env:AUTO_MAS_SENTRY_DSN)) {
        $ldflags += "-X github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/telemetry.BuildSentryDSN=$($env:AUTO_MAS_SENTRY_DSN)"
    }
    & go build -trimpath -buildvcs=false -ldflags ($ldflags -join ' ') -o $binary ./cmd/auto-mas-runtime
    if ($LASTEXITCODE -ne 0) { throw 'runtime release build failed' }
}
finally {
    Pop-Location
    # 只删除本脚本生成的单个资源，普通开发构建不带发布版本。
    if (Test-Path -LiteralPath $resource) { Remove-Item -LiteralPath $resource }
}
& (Join-Path $PSScriptRoot 'verify-runtime-release.ps1') -BinaryPath $binary -Tag $Tag -SourceCommit $SourceCommit -BuildDate $BuildDate
