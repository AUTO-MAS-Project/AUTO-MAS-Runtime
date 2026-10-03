#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Tag,
    [Parameter(Mandatory)][ValidatePattern('^[0-9a-f]{40}$')][string]$SourceCommit,
    [Parameter(Mandatory)][ValidatePattern('^[1-9][0-9]*-[1-9][0-9]*$')][string]$RequestID,
    [Parameter(Mandatory)][ValidatePattern('^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$')][string]$BuildDate,
    [Parameter(Mandatory)][string]$OutputPath,
    [string]$ApiUrl = 'https://api.github.com',
    [ValidateRange(1, 7200)][int]$TimeoutSeconds = 5400,
    [ValidateRange(1, 60)][int]$PollSeconds = 10
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if ($Tag -cnotmatch '^v[A-Za-z0-9._-]+$' -or $Tag.Length -gt 128 -or
    $Tag.Contains('..') -or $Tag.EndsWith('.') -or $Tag.EndsWith('.lock')) {
    throw 'invalid runtime release tag'
}
if ([string]::IsNullOrWhiteSpace($env:AUTO_MAS_SIGNING_TOKEN)) {
    throw 'missing AUTO_MAS_SIGNING_TOKEN'
}
$api = [Uri]$ApiUrl
if ($api.Scheme -ne 'https' -and -not ($api.Scheme -eq 'http' -and $api.IsLoopback)) {
    throw 'signing API requires HTTPS'
}
$repo = 'AUTO-MAS-Project/AUTO-MAS'
$workflow = 'build-sign-runtime.yml'
$base = "$($ApiUrl.TrimEnd('/'))/repos/$repo"
$headers = @{
    Authorization = "Bearer $($env:AUTO_MAS_SIGNING_TOKEN)"
    Accept = 'application/vnd.github+json'
    'X-GitHub-Api-Version' = '2022-11-28'
}

function Invoke-SigningAPI {
    param([string]$Path, [string]$Method = 'Get', [string]$Body)
    $arguments = @{
        Uri = "$base/$Path"; Headers = $headers; Method = $Method
        TimeoutSec = 30; SkipHttpErrorCheck = $true
    }
    if ($Body) { $arguments.Body = $Body; $arguments.ContentType = 'application/json' }
    try { $response = Invoke-WebRequest @arguments }
    catch { throw "signing API transport failed ($Method $Path)" }
    if ($response.StatusCode -notin @(200, 204)) {
        # 不回显响应体或异常详情，避免 API 凭据和外部内容进入日志。
        throw "signing API failed ($Method $Path, HTTP $($response.StatusCode))"
    }
    if ($response.StatusCode -eq 204) { return $null }
    return $response.Content | ConvertFrom-Json -Depth 32
}

$head = Invoke-SigningAPI -Path 'git/ref/heads/main'
$expectedHead = [string]$head.object.sha
if ($head.object.type -cne 'commit' -or $expectedHead -cnotmatch '^[0-9a-f]{40}$') {
    throw 'main signing workflow ref is invalid'
}
$started = [DateTimeOffset]::UtcNow.AddMinutes(-1).ToString('yyyy-MM-ddTHH:mm:ssZ')
$body = @{
    ref = 'main'
    inputs = @{ runtime_tag = $Tag; runtime_commit = $SourceCommit; request_id = $RequestID; build_date = $BuildDate }
} | ConvertTo-Json -Depth 4 -Compress
$null = Invoke-SigningAPI -Path "actions/workflows/$workflow/dispatches" -Method Post -Body $body
$deadline = [DateTimeOffset]::UtcNow.AddSeconds($TimeoutSeconds)
$run = $null
$queryDate = [Uri]::EscapeDataString(">=$started")
while ([DateTimeOffset]::UtcNow -lt $deadline) {
    if ($null -eq $run) {
        $candidates = @()
        # 分页扫描同一时间窗，不能把无关请求挤到下一页后误判为超时。
        for ($page = 1; $page -le 10; $page++) {
            $response = Invoke-SigningAPI -Path "actions/workflows/$workflow/runs?event=workflow_dispatch&branch=main&created=$queryDate&per_page=100&page=$page"
            $items = @($response.workflow_runs)
            $candidates += @($items | Where-Object { $_.display_title -ceq "Runtime signing $RequestID" })
            if ($items.Count -lt 100) { break }
            if ($page -eq 10) { throw 'too many signing runs to resolve request identity' }
        }
        if ($candidates.Count -gt 1) { throw 'multiple signing runs match the request' }
        if ($candidates.Count -eq 1) { $run = $candidates[0] }
    }
    if ($null -ne $run) {
        if ([string]$run.id -cnotmatch '^[1-9][0-9]*$' -or
            $run.repository.full_name -cne $repo -or $run.event -cne 'workflow_dispatch' -or
            $run.path -cne ".github/workflows/$workflow" -or $run.head_branch -cne 'main' -or
            $run.head_sha -cne $expectedHead -or $run.display_title -cne "Runtime signing $RequestID" -or
            [string]$run.run_attempt -cnotmatch '^[1-9][0-9]*$') {
            throw 'signing run origin does not match the request'
        }
        if ($run.status -ceq 'completed') {
            if ($run.conclusion -cne 'success') { throw 'main repository signing run did not succeed' }
            break
        }
    }
    Start-Sleep -Seconds $PollSeconds
    if ($null -ne $run) { $run = Invoke-SigningAPI -Path "actions/runs/$($run.id)" }
}
if ($null -eq $run -or $run.status -cne 'completed') { throw 'timed out waiting for runtime signing' }
if ($run.conclusion -cne 'success') { throw 'main repository signing run did not succeed' }
$response = Invoke-SigningAPI -Path "actions/runs/$($run.id)/artifacts?per_page=100"
$artifacts = @($response.artifacts | Where-Object { $_.name -ceq "runtime-signed-$RequestID" })
if ($artifacts.Count -ne 1 -or $artifacts[0].expired -ne $false -or
    [string]$artifacts[0].id -cnotmatch '^[1-9][0-9]*$' -or
    $artifacts[0].workflow_run.id -ne $run.id -or
    $artifacts[0].workflow_run.head_sha -cne $expectedHead) {
    throw 'signed artifact identity is missing, expired or ambiguous'
}
@("run_id=$($run.id)", "run_attempt=$($run.run_attempt)", "artifact_id=$($artifacts[0].id)") |
    Add-Content -LiteralPath $OutputPath -Encoding utf8NoBOM
Write-Host "Runtime signing completed: $repo/actions/runs/$($run.id)"
