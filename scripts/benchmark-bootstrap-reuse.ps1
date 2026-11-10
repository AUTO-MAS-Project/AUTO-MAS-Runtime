param(
    [Parameter(Mandatory)][string]$SourceRoot,
    [string]$BaseRef = 'HEAD',
    [ValidateRange(3, 20)][int]$Pairs = 5,
    [string]$OutputRoot = (Join-Path $env:TEMP ('mas-bootstrap-reuse-' + [guid]::NewGuid().ToString('N')))
)
$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$source = (Resolve-Path -LiteralPath $SourceRoot).Path.TrimEnd('\')
$output = [IO.Path]::GetFullPath($OutputRoot).TrimEnd('\')
if ($output -eq $source -or $output.StartsWith($source + '\', [StringComparison]::OrdinalIgnoreCase)) {
    throw 'Benchmark output must be outside the source installation'
}
if (Test-Path -LiteralPath $output) { throw 'Benchmark output must be a new directory' }
New-Item -ItemType Directory -Path $output | Out-Null
$appRoot = Join-Path $output 'app'
New-Item -ItemType Directory -Path $appRoot | Out-Null
$baseline = & git -C $repoRoot rev-parse $BaseRef
if ($LASTEXITCODE -ne 0) { throw 'Cannot resolve baseline' }
$overlayMap = @{}
foreach ($relative in @('internal/cli/bootstrap.go', 'internal/cli/version.go')) {
    $content = & git -C $repoRoot show ('{0}:{1}' -f $baseline, $relative)
    if ($LASTEXITCODE -ne 0) { throw 'Cannot read baseline source' }
    $saved = Join-Path $output ([IO.Path]::GetFileName($relative))
    [IO.File]::WriteAllText($saved, ($content -join "`n") + "`n", [Text.UTF8Encoding]::new($false))
    $overlayMap[(Join-Path $repoRoot $relative)] = $saved
}
$overlayPath = Join-Path $output 'baseline-overlay.json'
@{ Replace = $overlayMap } | ConvertTo-Json | Set-Content -LiteralPath $overlayPath -Encoding utf8
$oldExe = Join-Path $output 'baseline.exe'
$newExe = Join-Path $output 'candidate.exe'
Push-Location $repoRoot
try {
    & go build -buildvcs=false -overlay $overlayPath -o $oldExe ./cmd/auto-mas-runtime
    if ($LASTEXITCODE -ne 0) { throw 'Baseline build failed' }
    & go build -buildvcs=false -o $newExe ./cmd/auto-mas-runtime
    if ($LASTEXITCODE -ne 0) { throw 'Candidate build failed' }
} finally { Pop-Location }

# 只复制受管程序文件，不复制用户配置、任务、后端事务或指向其他目录的 Junction。
$stateSource = Join-Path $source 'runtime-state/environment.json'
$sourceStateBytes = [IO.File]::ReadAllBytes($stateSource)
$sourceStateHash = [Convert]::ToHexString([Security.Cryptography.SHA256]::HashData($sourceStateBytes))
foreach ($relative in @('repo', 'runtime/tools', 'runtime/environment/python', 'runtime/environment/venv')) {
    $from = Join-Path $source $relative
    $to = Join-Path $appRoot $relative
    & robocopy $from $to /E /COPY:DAT /R:1 /W:1 /XJ /NFL /NDL /NP /NJH /NJS | Out-Null
    if ($LASTEXITCODE -ge 8) { throw ('Snapshot copy failed: ' + $relative) }
}
if ((Get-FileHash -LiteralPath $stateSource -Algorithm SHA256).Hash -ne $sourceStateHash) {
    throw 'Source environment changed during snapshot; rerun with a stable source'
}
$stateDir = Join-Path $appRoot 'runtime-state'
New-Item -ItemType Directory -Path $stateDir | Out-Null
[IO.File]::WriteAllBytes((Join-Path $stateDir 'environment.json'), $sourceStateBytes)
$initial = Get-Content -LiteralPath (Join-Path $stateDir 'environment.json') -Raw | ConvertFrom-Json
$version = $initial.lastSuccessful.version
if ($initial.status -ne 'ready_to_start' -or -not $version) { throw 'Source must be a prepared environment' }
$venvConfig = Join-Path $appRoot 'runtime/environment/venv/pyvenv.cfg'
$cfg = Get-Content -LiteralPath $venvConfig -Raw
$cfg = $cfg.Replace($source, $appRoot)
$homeSetting = ($cfg -split "`r?`n" | Where-Object { $_ -match '^home\s*=' })
if ($homeSetting.Count -ne 1) { throw 'Snapshot venv must have one Python home' }
$pythonHome = [IO.Path]::GetFullPath(($homeSetting -split '=', 2)[1].Trim())
if (-not $pythonHome.StartsWith((Join-Path $appRoot 'runtime/environment/python') + '\', [StringComparison]::OrdinalIgnoreCase)) {
    throw 'Snapshot venv must use its isolated managed Python'
}
Set-Content -LiteralPath $venvConfig -Value $cfg -Encoding utf8
Write-Output ('Isolated snapshot: ' + $appRoot)

function Measure-Bootstrap {
    param([string]$Exe, [bool]$Reuse, [string]$Label)
    $stdoutPath = Join-Path $output ($Label + '.ndjson')
    $stderrPath = Join-Path $output ($Label + '.stderr.txt')
    $arguments = @('--app-root', $appRoot, '--output', 'ndjson', 'bootstrap', '--version', $version)
    if ($Reuse) { $arguments += '--reuse-ready-environment' }
    $watch = [Diagnostics.Stopwatch]::StartNew()
    & $Exe @arguments > $stdoutPath 2> $stderrPath
    $exitCode = $LASTEXITCODE
    $watch.Stop()
    $events = @(Get-Content -LiteralPath $stdoutPath | ForEach-Object { $_ | ConvertFrom-Json })
    $result = @($events | Where-Object type -eq 'result')[-1]
    if ($exitCode -ne 0 -or -not $result.success) {
        throw ('Bootstrap failed in {0}: exit={1}, code={2}; see {3}' -f $Label, $exitCode, $result.code, $stdoutPath)
    }
    if ($result.details.version -cne $version -or $result.details.commit -cne $initial.lastSuccessful.commit) {
        throw ('Bootstrap identity changed in {0}; expected {1}/{2}, got {3}/{4}; see {5}' -f $Label, $version, $initial.lastSuccessful.commit, $result.details.version, $result.details.commit, $stdoutPath)
    }
    $pythonPrepared = @($events | Where-Object stage -eq 'python.install').Count -gt 0
    $dependenciesSynced = @($events | Where-Object stage -eq 'dependencies.sync').Count -gt 0
    $checked = @($events | Where-Object stage -eq 'dependencies.check').Count -gt 0
    if ($Reuse -and (-not $result.details.unchanged -or $pythonPrepared -or $dependenciesSynced -or -not $checked)) {
        throw ('Candidate did not prove healthy reuse: ' + $stdoutPath)
    }
    if (-not $Reuse -and (-not $pythonPrepared -or -not $dependenciesSynced)) {
        throw ('Control did not perform full preparation: ' + $stdoutPath)
    }
    $row = [pscustomobject]@{
        Label = $Label; Reuse = $Reuse; DurationMs = [math]::Round($watch.Elapsed.TotalMilliseconds, 2)
        Version = $result.details.version; Commit = $result.details.commit
        ProbeEvents = @($events | Where-Object stage -eq 'network.probe').Count
        PythonPrepared = $pythonPrepared; DependenciesSynced = $dependenciesSynced
        ReadinessChecked = $checked; LogPath = $stdoutPath
    }
    Write-Host ('{0}: {1} ms, probes={2}' -f $Label, $row.DurationMs, $row.ProbeEvents)
    return $row
}

$warmups = @()
$warmups += Measure-Bootstrap $oldExe $false 'warmup-baseline'
$warmups += Measure-Bootstrap $newExe $true 'warmup-candidate'
$rows = @()
for ($pair = 1; $pair -le $Pairs; $pair++) {
    if ($pair % 2 -eq 1) {
        $rows += Measure-Bootstrap $oldExe $false ('{0}-baseline' -f $pair)
        $rows += Measure-Bootstrap $newExe $true ('{0}-candidate' -f $pair)
    } else {
        $rows += Measure-Bootstrap $newExe $true ('{0}-candidate' -f $pair)
        $rows += Measure-Bootstrap $oldExe $false ('{0}-baseline' -f $pair)
    }
}
function Get-Median {
    param([double[]]$Values)
    $sorted = @($Values | Sort-Object)
    $middle = [int][math]::Floor($sorted.Count / 2)
    if ($sorted.Count % 2 -eq 1) { return $sorted[$middle] }
    return ($sorted[$middle - 1] + $sorted[$middle]) / 2
}
$oldMedian = Get-Median @($rows | Where-Object { -not $_.Reuse } | ForEach-Object DurationMs)
$newMedian = Get-Median @($rows | Where-Object Reuse | ForEach-Object DurationMs)
$savedMs = [math]::Round($oldMedian - $newMedian, 2)
$metadata = [ordered]@{
    Baseline = $baseline; Version = $version; Commit = $initial.lastSuccessful.commit
    AppRoot = $appRoot; BaselineSHA256 = (Get-FileHash -LiteralPath $oldExe).Hash
    CandidateSHA256 = (Get-FileHash -LiteralPath $newExe).Hash
    Warmups = $warmups; Samples = $rows; BaselineMedianMs = $oldMedian; CandidateMedianMs = $newMedian; SavedMs = $savedMs
}
$metadata | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath (Join-Path $output 'comparison.json') -Encoding utf8
$report = @(
    '# Bootstrap 健康环境复用实测', '',
    ('基线 {0}；目标 {1}，Commit {2}。' -f $baseline, $version, $initial.lastSuccessful.commit),
    '同一隔离快照、真实 uv、交替顺序；预热不计入中位数。不启动后端，不修改安装版。', '',
    '| 样本 | 毫秒 | 探针事件数 | Python 准备 | 依赖同步 | 就绪复验 |',
    '| --- | ---: | ---: | --- | --- | --- |'
)
foreach ($row in $rows) {
    $report += '| {0} | {1} | {2} | {3} | {4} | {5} |' -f $row.Label, $row.DurationMs, $row.ProbeEvents, $row.PythonPrepared, $row.DependenciesSynced, $row.ReadinessChecked
}
$report += ''
$report += '基线中位数 {0:N2} ms；候选 {1:N2} ms；减少 {2:N2} ms。' -f $oldMedian, $newMedian, $savedMs
$report += '仅代表该机器健康环境的完整 bootstrap，不等于整机启动或首次安装收益。源码校验与 Git 网络阶段仍保留，约 24 秒异常未由此实验解释。'
$report | Set-Content -LiteralPath (Join-Path $output 'comparison.md') -Encoding utf8
Write-Output ('Report: ' + (Join-Path $output 'comparison.md'))
