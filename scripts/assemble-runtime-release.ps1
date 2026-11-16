#Requires -Version 7.0
[CmdletBinding()]
param([Parameter(Mandatory)][ValidatePattern('^v[A-Za-z0-9._-]+$')][string]$Tag,
      [Parameter(Mandatory)][string]$AssetsDirectory)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$expected = @("auto-mas-runtime-$Tag.exe")
foreach ($os in @('linux','darwin')) {
    foreach ($arch in @('amd64','arm64')) { $expected += "auto-mas-runtime_${Tag}_${os}_${arch}" }
}
$actual = @(Get-ChildItem -LiteralPath $AssetsDirectory -File | Where-Object Name -cne 'SHA256SUMS.txt')
if ($actual.Count -ne 5 -or @(Compare-Object ($expected | Sort-Object) ($actual.Name | Sort-Object)).Count -ne 0) {
    throw 'release must contain exactly the signed Windows executable and four POSIX binaries'
}
$lines = foreach ($name in ($expected | Sort-Object)) {
    $path = Join-Path $AssetsDirectory $name
    if ((Get-Item -LiteralPath $path).Length -le 0) { throw "empty release asset: $name" }
    $hash = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
    "$hash  $name"
}
$lines | Set-Content -LiteralPath (Join-Path $AssetsDirectory 'SHA256SUMS.txt') -Encoding ascii
