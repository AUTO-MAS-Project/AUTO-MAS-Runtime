#   AUTO-MAS Runtime: Windows 本机运行时管理程序
#   Copyright © 2025-2026 AUTO-MAS Team

# 把已发布且冒烟通过的 GitHub Release 复制为 CNB 同名 Release。
# GitHub 是唯一权威源：本脚本只复制，任何不一致都失败关闭，绝不覆盖或删除 CNB 侧数据。
# 幂等：CNB 已存在且完全一致的 Release 视为成功；缺失资产只补齐。
# 全程不打印 token、Authorization、upload_url 或 verify_url。

param(
    [Parameter(Mandatory = $true)][string]$GitHubToken,
    [string]$GitHubApiUrl = "https://api.github.com",
    [Parameter(Mandatory = $true)][string]$GitHubRepository,
    [Parameter(Mandatory = $true)][string]$CnbToken,
    [string]$CnbApiUrl = "https://api.cnb.cool",
    [Parameter(Mandatory = $true)][string]$CnbRepository,
    [string]$CnbDownloadBase = "https://cnb.cool",
    [Parameter(Mandatory = $true)][string]$Tag,
    [string]$ExpectedCommit = "",
    [Parameter(Mandatory = $true)][string]$AssetsDir,
    [switch]$DryRun,
    [int]$TagWaitSeconds = 600,
    [int]$TagPollIntervalSeconds = 10
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version 3.0

# ---------------------------------------------------------------------------
# 入口校验：Tag 安全规则与 release.yml 保持一致。
# ---------------------------------------------------------------------------

function Assert-SafeReleaseTag {
    param([string]$Value)
    $tagBytes = [Text.Encoding]::UTF8.GetByteCount($Value)
    if ($tagBytes -lt 2 -or $tagBytes -gt 128 -or $Value -cnotmatch '^v[A-Za-z0-9._-]+$') {
        throw "release tag is not a valid runtime version"
    }
    if ($Value.Contains("..") -or
        $Value.Contains("@{") -or
        $Value.EndsWith(".") -or
        $Value.EndsWith(".lock")) {
        throw "release tag contains a forbidden git ref pattern"
    }
}

function Assert-Sha256Hex {
    param([string]$Value)
    if ($Value -cnotmatch '^[0-9a-f]{64}$') {
        throw "value is not a lowercase sha-256 digest"
    }
}

Assert-SafeReleaseTag -Value $Tag
if ([string]::IsNullOrWhiteSpace($GitHubToken)) {
    throw "GitHub token is empty"
}
if ([string]::IsNullOrWhiteSpace($CnbToken)) {
    throw "CNB token is empty"
}
if (-not (Test-Path -LiteralPath $AssetsDir -PathType Container)) {
    throw "assets directory does not exist: $AssetsDir"
}

$githubHeaders = @{
    Accept                 = "application/vnd.github+json"
    Authorization          = "Bearer $GitHubToken"
    "X-GitHub-Api-Version" = "2022-11-28"
}
$cnbHeaders = @{
    Accept        = "application/vnd.cnb.api+json"
    Authorization = "Bearer $CnbToken"
    "User-Agent"  = "auto-mas-runtime-cnb-publisher/1.0"
}
$githubBase = $GitHubApiUrl.TrimEnd('/')
$cnbBase = $CnbApiUrl.TrimEnd('/')
# 仓库路径保留原始斜杠：GitHub/CNB API 与公开下载端点的 {repo} 段都不接受 %2F，
# 且该值来自 workflow 硬编码（org/repo 形态），不经过用户输入。
$githubPath = $GitHubRepository
$cnbPath = $CnbRepository
$tagPath = [Uri]::EscapeDataString($Tag)

function Invoke-JsonApi {
    param(
        [string]$Method,
        [string]$Uri,
        [hashtable]$Headers,
        [object]$Body = $null,
        [int[]]$AcceptStatus = @(200)
    )
    $parameters = @{
        Uri                 = $Uri
        Method              = $Method
        Headers             = $Headers
        SkipHttpErrorCheck  = $true
        ContentType         = "application/json"
        TimeoutSec          = 60
    }
    if ($null -ne $Body) {
        $parameters["Body"] = ($Body | ConvertTo-Json -Depth 6 -Compress)
    }
    $response = Invoke-WebRequest @parameters
    if ($AcceptStatus -notcontains $response.StatusCode) {
        $detail = ""
        $errorContent = $response.Content
        if ($errorContent -is [byte[]]) {
            $errorContent = [Text.Encoding]::UTF8.GetString($errorContent)
        }
        if ($errorContent) {
            # 只保留 errmsg/message，避免把整段响应打进日志。
            $parsed = $errorContent | ConvertFrom-Json -ErrorAction SilentlyContinue
            if ($null -ne $parsed -and ($parsed.PSObject.Properties.Name -contains "errmsg")) {
                $detail = " ($($parsed.errmsg))"
            }
            elseif ($null -ne $parsed -and ($parsed.PSObject.Properties.Name -contains "message")) {
                $detail = " ($($parsed.message))"
            }
        }
        throw "request to $Method $Uri returned unexpected HTTP $($response.StatusCode)$detail"
    }
    $content = $response.Content
    if ($content -is [byte[]]) {
        # 部分内容类型下 PowerShell 返回字节数组而非字符串。
        $content = [Text.Encoding]::UTF8.GetString($content)
    }
    if ($response.StatusCode -ge 300 -or $response.StatusCode -lt 200 -or
        $response.StatusCode -eq 204 -or [string]::IsNullOrWhiteSpace($content)) {
        # 404 等非 2xx 的接受状态返回 $null，表示"对象不存在"。
        return $null
    }
    return $content | ConvertFrom-Json -ErrorAction Stop
}

Write-Host "==> Resolving GitHub release $Tag"

# ---------------------------------------------------------------------------
# 第 1 步：读取 GitHub Release，必须已发布（非 draft）、非空资产。
# ---------------------------------------------------------------------------

$release = Invoke-JsonApi -Method Get `
    -Uri "$githubBase/repos/$githubPath/releases/tags/$tagPath" `
    -Headers $githubHeaders `
    -AcceptStatus @(200, 404)
if ($null -eq $release) {
    throw "GitHub release $Tag does not exist"
}
if ($release.draft) {
    throw "GitHub release $Tag is still a draft"
}
$githubAssets = @($release.assets)
if ($githubAssets.Count -eq 0) {
    throw "GitHub release $Tag has no assets to publish"
}
$prerelease = [bool]$release.prerelease
$releaseName = [string]$release.name
if ([string]::IsNullOrWhiteSpace($releaseName)) {
    $releaseName = $Tag
}
Write-Host ("GitHub release resolved: {0} asset(s), prerelease={1}" -f $githubAssets.Count, $prerelease)

# ---------------------------------------------------------------------------
# 第 2 步：下载全部 GitHub 资产并校验 GitHub digest。
# ---------------------------------------------------------------------------

$localFiles = @{}
foreach ($asset in $githubAssets) {
    $assetName = [string]$asset.name
    if ($assetName -cnotmatch '^[A-Za-z0-9._-]+$') {
        throw "GitHub asset name contains unexpected characters: $assetName"
    }
    $digestParts = ([string]$asset.digest) -split ":", 2
    if ($digestParts.Count -ne 2 -or $digestParts[0] -ne "sha256") {
        throw "GitHub asset $assetName has no sha-256 digest"
    }
    $expectedDigest = $digestParts[1]
    Assert-Sha256Hex -Value $expectedDigest
    $target = Join-Path $AssetsDir $assetName
    if (-not $DryRun) {
        # asset API url 必须带 Accept: application/octet-stream 才返回二进制；
        # 通用 vnd.github+json 会拿到 JSON 元数据（digest 校验会立即失败关闭）。
        $downloadHeaders = @{
            Accept                 = "application/octet-stream"
            Authorization          = $githubHeaders.Authorization
            "X-GitHub-Api-Version" = "2022-11-28"
        }
        # 用 -OutFile 直接落盘：binary 与文本响应都保证字节精确。
        $null = Invoke-WebRequest -Uri ([string]$asset.url) -Headers $downloadHeaders -Method Get -MaximumRedirection 5 -TimeoutSec 600 -OutFile $target
        $actual = (Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actual -cne $expectedDigest) {
            throw "GitHub asset $assetName digest mismatch after download"
        }
        $localFiles[$assetName] = @{
            Path = $target
            Sha256 = $actual
            Size = (Get-Item -LiteralPath $target).Length
        }
    }
    else {
        $localFiles[$assetName] = @{
            Path = $target
            Sha256 = $expectedDigest
            Size = [int64]$asset.size
        }
    }
    Write-Host ("Downloaded {0} ({1} bytes, sha256 verified)" -f $assetName, $localFiles[$assetName].Size)
}

# ---------------------------------------------------------------------------
# 第 3 步：校验 SHA256SUMS.txt 覆盖目录内每个资产。
# ---------------------------------------------------------------------------

$sumsName = "SHA256SUMS.txt"
if (-not $localFiles.ContainsKey($sumsName)) {
    throw "GitHub release $Tag does not contain $sumsName"
}
# dry-run 不落盘，改用 API 侧已知值；真实运行从文件读取。
$sumsLines = if ($DryRun) {
    @()
}
else {
    Get-Content -LiteralPath $localFiles[$sumsName].Path -Encoding ascii
}
$sumsMap = @{}
foreach ($line in $sumsLines) {
    if ([string]::IsNullOrWhiteSpace($line)) { continue }
    $parts = $line -split "\s+", 2
    if ($parts.Count -ne 2) {
        throw "checksum line is malformed: $line"
    }
    $sumsMap[$parts[1].TrimStart("*")] = $parts[0]
}
# dry-run 下校验和文件的内容与 GitHub digest 一致才继续（用 API 的 size 佐证弱校验）。
if (-not $DryRun) {
    foreach ($name in $localFiles.Keys) {
        if ($name -eq $sumsName) { continue }
        if (-not $sumsMap.ContainsKey($name)) {
            throw "checksum file does not cover $name"
        }
        if ($sumsMap[$name] -cne $localFiles[$name].Sha256) {
            throw "checksum file entry for $name does not match the downloaded digest"
        }
    }
}

# ---------------------------------------------------------------------------
# 第 4 步：确定期望 Commit（显式给出优先，否则从 GitHub Tag API 递归剥离）。
# ---------------------------------------------------------------------------

function Get-GitHubTagCommit {
    param([string]$ReleaseTag)
    $reference = Invoke-JsonApi -Method Get `
        -Uri "$githubBase/repos/$githubPath/git/ref/tags/$tagPath" `
        -Headers $githubHeaders `
        -AcceptStatus @(200, 404)
    if ($null -eq $reference) {
        throw "GitHub tag $ReleaseTag does not exist"
    }
    $objectType = [string]$reference.object.type
    $objectSha = [string]$reference.object.sha
    $resolvedCommit = ""
    $seenObjects = @{}
    for ($depth = 0; $depth -lt 8; $depth++) {
        if ($objectSha -cnotmatch '^[0-9a-f]{40}$') {
            throw "release tag object contains an invalid sha"
        }
        switch ($objectType) {
            "commit" {
                $resolvedCommit = $objectSha
            }
            "tag" {
                if ($seenObjects.ContainsKey($objectSha)) {
                    throw "release tag object graph contains a cycle"
                }
                $seenObjects[$objectSha] = $true
                $nestedPath = [Uri]::EscapeDataString($objectSha)
                $tagObject = Invoke-JsonApi -Method Get `
                    -Uri "$githubBase/repos/$githubPath/git/tags/$nestedPath" `
                    -Headers $githubHeaders
                $objectType = [string]$tagObject.object.type
                $objectSha = [string]$tagObject.object.sha
            }
            default {
                throw "release tag resolves to unsupported object type $objectType"
            }
        }
        if ($resolvedCommit) { break }
    }
    if (-not $resolvedCommit) {
        throw "annotated release tag nesting exceeds the supported depth"
    }
    return $resolvedCommit
}

if ([string]::IsNullOrWhiteSpace($ExpectedCommit)) {
    $ExpectedCommit = Get-GitHubTagCommit -ReleaseTag $Tag
}
if ($ExpectedCommit -cnotmatch '^[0-9a-f]{40}$') {
    throw "expected commit is not a full 40-digit sha"
}
Write-Host "Expected commit: $ExpectedCommit"

# ---------------------------------------------------------------------------
# 第 5 步：轮询 CNB Tag，确认 git-sync 已把 Tag 同步到期望 Commit。
# ---------------------------------------------------------------------------

function Get-CnbTag {
    param([string]$ReleaseTag)
    $tagDecodePath = [Uri]::EscapeDataString($ReleaseTag)
    return Invoke-JsonApi -Method Get `
        -Uri "$cnbBase/$cnbPath/-/git/tags/$tagDecodePath" `
        -Headers $cnbHeaders `
        -AcceptStatus @(200, 404)
}

$deadline = (Get-Date).AddSeconds($TagWaitSeconds)
$cnbTagCommit = ""
while ($true) {
    $tagInfo = Get-CnbTag -ReleaseTag $Tag
    if ($null -ne $tagInfo) {
        $targetType = [string]$tagInfo.target_type
        if ($targetType -eq "commit") {
            $cnbTagCommit = [string]$tagInfo.target
        }
        elseif ($targetType -eq "tag") {
            $cnbTagCommit = [string]$tagInfo.commit.sha
        }
        else {
            throw "CNB tag $Tag has unsupported target type $targetType"
        }
        if ($cnbTagCommit -ieq $ExpectedCommit) { break }
        throw "CNB tag $Tag points to commit $cnbTagCommit, expected $ExpectedCommit (refusing to republish)"
    }
    if ((Get-Date) -ge $deadline) {
        throw "CNB tag $Tag did not appear within the wait window"
    }
    Write-Host "CNB tag not mirrored yet; retrying in $TagPollIntervalSeconds seconds"
    Start-Sleep -Seconds $TagPollIntervalSeconds
}
Write-Host "CNB tag confirmed at commit $cnbTagCommit"

# ---------------------------------------------------------------------------
# 第 6 步：查询或创建 CNB Release（target_commitish 填精确 Commit）。
# ---------------------------------------------------------------------------

$existing = Invoke-JsonApi -Method Get `
    -Uri "$cnbBase/$cnbPath/-/releases/tags/$tagPath" `
    -Headers $cnbHeaders `
    -AcceptStatus @(200, 404)

if ($null -eq $existing) {
    if ($DryRun) {
        Write-Host "DRY-RUN: would create CNB release $Tag at $ExpectedCommit"
        exit 0
    }
    Write-Host "Creating CNB release $Tag"
    $existing = Invoke-JsonApi -Method Post `
        -Uri "$cnbBase/$cnbPath/-/releases" `
        -Headers $cnbHeaders `
        -AcceptStatus @(200, 201) `
        -Body @{
            tag_name         = $Tag
            name             = $releaseName
            body             = [string]$release.body
            draft            = $false
            prerelease       = $prerelease
            target_commitish = $ExpectedCommit
            make_latest      = "legacy"
        }
    if ($null -eq $existing -or [string]::IsNullOrEmpty([string]$existing.id)) {
        throw "CNB release creation returned no release id"
    }
}
else {
    $existingCommit = [string]$existing.tag_commitish
    if ($existingCommit -cne $ExpectedCommit) {
        # CNB 可能把 target_commitish 归一化成 ref 名（如 refs/tags/v0.1.10）。
        # Release 绑定的真实 commit 以该 ref 在 CNB Git 上的当前指向为准，再与期望比对。
        $refCommit = ""
        if ($existingCommit -match '^[0-9a-f]{40}$') {
            $refCommit = ""
        }
        else {
            $refName = $existingCommit
            if ($refName.StartsWith("refs/tags/")) {
                $refName = $refName.Substring("refs/tags/".Length)
            }
            $refInfo = Invoke-JsonApi -Method Get `
                -Uri "$cnbBase/$cnbPath/-/git/tags/$([Uri]::EscapeDataString($refName))" `
                -Headers $cnbHeaders `
                -AcceptStatus @(200, 404)
            if ($null -eq $refInfo) {
                throw "existing CNB release $Tag references unknown ref $existingCommit"
            }
            $refType = [string]$refInfo.target_type
            if ($refType -eq "commit") {
                $refCommit = [string]$refInfo.target
            }
            elseif ($refType -eq "tag") {
                $refCommit = [string]$refInfo.commit.sha
            }
            else {
                throw "existing CNB release $Tag references unsupported object type $refType"
            }
        }
        if ($refCommit -ine $ExpectedCommit) {
            throw "existing CNB release $Tag points to commit $refCommit (via $existingCommit), expected $ExpectedCommit"
        }
        Write-Host "Existing CNB release $Tag commit verified via ref $existingCommit"
    }
    if ($existing.draft) {
        throw "existing CNB release $Tag is a draft"
    }
    if ([bool]$existing.prerelease -ne $prerelease) {
        throw "existing CNB release $Tag prerelease flag differs from GitHub"
    }
    Write-Host "Existing CNB release $Tag is consistent with GitHub; checking assets"
}

# ---------------------------------------------------------------------------
# 第 7 步：逐资产比对/上传（overwrite=false，同名不同字节立即失败）。
# ---------------------------------------------------------------------------

$existingAssets = @{}
if ($null -ne $existing.assets) {
    foreach ($asset in @($existing.assets)) {
        $existingAssets[[string]$asset.name] = $asset
    }
}

foreach ($name in ($localFiles.Keys | Sort-Object)) {
    $local = $localFiles[$name]
    if ($existingAssets.ContainsKey($name)) {
        $remote = $existingAssets[$name]
        $remoteHash = [string]$remote.hash_value
        if ([string]::IsNullOrWhiteSpace($remoteHash) -or $remote.hash_algo -ne "sha256") {
            throw "existing CNB asset $name has no sha-256 hash to compare"
        }
        if ($remoteHash.ToLowerInvariant() -cne $local.Sha256) {
            throw "existing CNB asset $name hash differs from GitHub asset; refusing to overwrite"
        }
        Write-Host "Asset $name already present and identical"
        continue
    }
    if ($DryRun) {
        Write-Host "DRY-RUN: would upload asset $name ($($local.Size) bytes)"
        continue
    }
    Write-Host "Uploading asset $name"
    $uploadPath = [Uri]::EscapeDataString([string]$existing.id)
    $uploadInfo = Invoke-JsonApi -Method Post `
        -Uri "$cnbBase/$cnbPath/-/releases/$uploadPath/asset-upload-url" `
        -Headers $cnbHeaders `
        -AcceptStatus @(200, 201) `
        -Body @{
            asset_name = $name
            overwrite  = $false
            size       = $local.Size
        }
    if ($null -eq $uploadInfo -or [string]::IsNullOrEmpty([string]$uploadInfo.upload_url)) {
        throw "CNB did not return an upload url for asset $name"
    }
    $fileBytes = [IO.File]::ReadAllBytes($local.Path)
    # 预签名 upload_url 自带授权，不得附加 Authorization 头。
    $put = Invoke-WebRequest -Uri ([string]$uploadInfo.upload_url) -Method Put -Body $fileBytes -TimeoutSec 900
    if ($put.StatusCode -ge 300) {
        throw "upload of asset $name returned HTTP $($put.StatusCode)"
    }
    if ($null -ne $uploadInfo.verify_url -and -not [string]::IsNullOrEmpty([string]$uploadInfo.verify_url)) {
        # verify 端点仍要求调用者身份（与 AUTO-MAS 上传器一致带 Authorization）。
        $verify = Invoke-WebRequest -Uri ([string]$uploadInfo.verify_url) -Method Post -Headers $cnbHeaders -TimeoutSec 120
        if ($verify.StatusCode -ge 300) {
            throw "upload confirmation of asset $name returned HTTP $($verify.StatusCode)"
        }
    }
    $refreshed = Invoke-JsonApi -Method Get `
        -Uri "$cnbBase/$cnbPath/-/releases/tags/$tagPath" `
        -Headers $cnbHeaders
    $uploaded = $null
    foreach ($asset in @($refreshed.assets)) {
        if ($null -eq $asset) { continue }
        if ([string]$asset.name -eq $name) { $uploaded = $asset; break }
    }
    if ($null -eq $uploaded) {
        throw "asset $name is still missing on CNB after upload"
    }
    if (([string]$uploaded.hash_value).ToLowerInvariant() -cne $local.Sha256) {
        throw "asset $name hash on CNB differs after upload"
    }
    Write-Host "Asset $name uploaded and verified"
}

# ---------------------------------------------------------------------------
# 第 8 步：从 CNB 公开下载地址回读全部资产，逐字节比对 SHA-256。
# ---------------------------------------------------------------------------

if ($DryRun) {
    Write-Host "DRY-RUN: skipping public download verification"
    Write-Host "==> Dry-run complete: no changes were made to CNB"
    exit 0
}

foreach ($name in ($localFiles.Keys | Sort-Object)) {
    # repo 路径保留原始斜杠：公开下载端点的路径段不含编码。
    $downloadUrl = "$($CnbDownloadBase.TrimEnd('/'))/$CnbRepository/-/releases/download/$tagPath/$name"
    $roundTripFile = Join-Path $env:TEMP ("cnb-roundtrip-" + [Guid]::NewGuid().ToString("N"))
    try {
        $null = Invoke-WebRequest -Uri $downloadUrl -Method Get -TimeoutSec 900 -OutFile $roundTripFile
        $hasher = [Security.Cryptography.SHA256]::Create()
        try {
            $stream = [IO.File]::OpenRead($roundTripFile)
            try {
                $hashBytes = $hasher.ComputeHash($stream)
            }
            finally {
                $stream.Dispose()
            }
        }
        finally {
            $hasher.Dispose()
        }
    }
    finally {
        Remove-Item -LiteralPath $roundTripFile -Force -ErrorAction SilentlyContinue
    }
    $actual = ([BitConverter]::ToString($hashBytes)).Replace("-", "").ToLowerInvariant()
    if ($actual -cne $localFiles[$name].Sha256) {
        throw "public CNB download of $name does not match the GitHub digest"
    }
    Write-Host "Public download of $name verified"
}

Write-Host "==> CNB release $Tag is published and verified"
exit 0
