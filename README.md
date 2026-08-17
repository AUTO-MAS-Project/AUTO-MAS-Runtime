# AUTO-MAS Runtime

AUTO-MAS Runtime 是 AUTO-MAS 的 Windows 本机运行时管理程序。它负责准备
受管环境、同步后端仓库、管理 uv/Python 与项目依赖，并启动和监督后端进程。

Runtime 不代理业务 HTTP/WebSocket，不管理 Python 插件依赖，也不负责自身更新。
正式发布首版只支持 Windows；实现进度以 [任务拆分](doc/任务拆分.md) 为准。

## 快速开始

### 直接运行已构建的 EXE

PowerShell 7 中，路径放在变量里时使用调用运算符 `&`：

```powershell
$runtime = (Resolve-Path '.\auto-mas-runtime.exe').Path
$appRoot = 'D:\AUTO-MAS'

& $runtime --app-root $appRoot version
$exitCode = $LASTEXITCODE
if ($exitCode -ne 0) { throw "Runtime exited with code $exitCode" }
```

查看所有命令：

```powershell
& $runtime --help
& $runtime bootstrap --help
& $runtime backend supervise --help
```

### 从源码运行

不需要先安装 Runtime；在仓库根目录执行：

```powershell
& go run .\cmd\auto-mas-runtime --help
if ($LASTEXITCODE -ne 0) { throw "help failed" }
```

### 构建 EXE

```powershell
New-Item -ItemType Directory -Path '.\dist' -Force | Out-Null
& go build -buildvcs=false -o '.\dist\auto-mas-runtime.exe' .\cmd\auto-mas-runtime
if ($LASTEXITCODE -ne 0) { throw "build failed" }
```

构建完成后，`dist\auto-mas-runtime.exe` 就是可直接调用的 Runtime。

## 调用格式

通用格式如下。为便于阅读，下面的示例都把全局选项放在命令前面：

```text
auto-mas-runtime [全局选项] <命令> [命令选项]
```

Runtime 有两类调用：

- 一次性命令：执行检查、准备、更新或修复后退出。
- `backend supervise`：启动并持续监督后端，直到后端停止、收到 `shutdown`
  或发生失败。

每次调用只执行一个顶层操作，不提供通用常驻 RPC 服务。`bootstrap` 只准备环境，
不会自动启动后端；准备成功后需要单独调用 `backend supervise`。

## 命令一览

| 命令 | 用途 | 是否修改受管内容 |
| --- | --- | --- |
| `version` | 查看 Runtime 与协议版本 | 否 |
| `doctor` | 只读检查本机运行环境 | 否 |
| `workspace check` | 检查受管后端仓库 | 否 |
| `workspace sync --version <版本>` | 同步受管后端仓库到目标版本 | 是 |
| `environment check` | 检查 uv 与受管 Python | 否 |
| `environment ensure` | 准备固定版本 uv | 是 |
| `environment repair` | 重新校验 uv、重装受管 Python；不重建 venv | 是 |
| `dependencies check` | 检查锁定依赖是否同步 | 否 |
| `dependencies sync` | 按锁文件同步主项目依赖 | 是 |
| `dependencies rebuild` | 重建受管 venv 并同步依赖 | 是 |
| `bootstrap --version <版本>` | 按顺序完成 uv、仓库、Python 和依赖准备 | 是 |
| `repair` | 完整修复 uv、Python、venv 和锁定依赖 | 是 |
| `cleanup` | 清理 Runtime 判定为可丢弃的缓存和临时内容 | 是 |
| `backend supervise --mode managed` | 启动并监督受管后端 | 运行期间管理进程 |
| `backend supervise --mode development --repo <目录>` | 启动并监督开发源码目录 | 不修改源码目录 |

`workspace sync` 只负责仓库，不会同步依赖，也不会启动或停止后端。需要准备
正式运行环境时，通常直接使用 `bootstrap`。

## 常用调用流程

### 正式安装模式（managed）

`--app-root` 是 Runtime 管理目录的根目录；Runtime 会在其下维护受管仓库、
工具、环境、日志和状态。

```powershell
$runtime = 'D:\Tools\auto-mas-runtime.exe'
$appRoot = 'D:\AUTO-MAS'
$version = 'v5.4.0-beta.5'

& $runtime --app-root $appRoot bootstrap --version $version
if ($LASTEXITCODE -ne 0) { throw "bootstrap failed" }

# bootstrap 不启动后端；启动后端需要单独调用：
& $runtime --app-root $appRoot backend supervise --mode managed
```

Runtime 只接收版本号，不接收任意 Git 分支或 Commit。版本必须以 `v` 开头，
例如 `v5.4.0-beta.5`。

### 开发模式（development）

开发模式监督开发者指定的源码目录，不会执行 `workspace sync`、依赖同步或源码
目录清理：

```powershell
& $runtime --app-root $appRoot backend supervise --mode development --repo 'D:\Github\AUTO-MAS'
```

源码目录需要已经具备可运行的 `main.py`、`pyproject.toml` 和 `.venv`。`--repo`
建议使用绝对路径；Runtime 不会替开发模式创建或同步源码目录中的环境。

### 只读诊断与修复

```powershell
# 只读诊断
& $runtime --app-root $appRoot doctor
& $runtime --app-root $appRoot workspace check
& $runtime --app-root $appRoot environment check
& $runtime --app-root $appRoot dependencies check

# 受管环境修复
& $runtime --app-root $appRoot repair

# 清理可丢弃缓存和临时内容
& $runtime --app-root $appRoot cleanup
```

## 全局选项

这些选项可以和支持它们的命令一起使用：

| 选项 | 默认值 | 说明 |
| --- | --- | --- |
| `--app-root <目录>` | 当前工作目录 | Runtime 受管目录根；建议显式传入绝对路径 |
| `--output <模式>` | `human` | 取值为 `human` 或 `ndjson` |
| `--protocol <版本>` | `1` | Runtime 协议版本；当前只支持 `1` |
| `--offline` | 关闭 | 禁止所有网络访问 |
| `--mirror <类型>=<键>` | 无 | 指定镜像源，可重复使用；类型为 `git`、`uv`、`python`、`package-index` |
| `--mirror-only` | 关闭 | 只使用配置的镜像源，不回退官方源 |

`--offline` 不能和 `--mirror` 或 `--mirror-only` 同时使用。网络相关操作在
离线模式下无法完成时会返回 `NETWORK_UNAVAILABLE`。

## 错误观测

正式发布可以通过可选的 GitHub Actions secret `AUTO_MAS_SENTRY_DSN` 启用 Sentry
错误观测。它必须是 Sentry 项目 DSN，不是 auth token；没有该 secret 时，观测保持
no-op。Runtime 只上报已清理的 `INTERNAL_ERROR` 和未预期的 panic，不启用 tracing
或日志转发，也不使用 PostHog/Umami。

临时本地运行可以在当前 PowerShell 7 会话设置相关环境变量；不要把 DSN 写入仓库、
脚本或提交历史。需要禁用观测时：

`powershell
$env:AUTO_MAS_TELEMETRY = 'disabled'
`

## 给 Electron 或其他程序调用

### PowerShell 参数数组

使用参数数组可以避免把命令拼成一整条 Shell 字符串：

```powershell
$runtimeArguments = @(
    '--app-root', 'D:\AUTO-MAS',
    '--output', 'ndjson',
    '--protocol', '1',
    'bootstrap',
    '--version', 'v5.4.0-beta.5'
)

& $runtime @runtimeArguments
$exitCode = $LASTEXITCODE
```

### Node.js / Electron

Electron 应使用 `spawn` 的可执行文件路径和参数数组，并分别读取 stdout、stderr：

```javascript
import { spawn } from 'node:child_process';

const child = spawn(runtimePath, [
  '--app-root', appRoot,
  '--output', 'ndjson',
  '--protocol', '1',
  'backend', 'supervise',
  '--mode', 'managed',
], {
  stdio: ['pipe', 'pipe', 'pipe'],
});

child.stdout.setEncoding('utf8');
child.stdout.on('data', (chunk) => {
  for (const line of chunk.split(/\r?\n/)) {
    if (line.trim()) {
      const event = JSON.parse(line);
      // 按 event.type、event.code 和 event.success 处理，不解析中文 message。
    }
  }
});

child.stderr.setEncoding('utf8');
child.stderr.on('data', (chunk) => {
  // Runtime 诊断信息。
});
```

不要通过 `cmd /c` 或 Shell 字符串拼接来启动 Runtime。机器调用方应同时检查：

- stdout 中的 NDJSON 事件；
- 最终 `result` 事件中的 `success`、`code`、`stage` 和 `details`；
- 进程退出码。

### stdin 控制命令

耗时命令支持逐行写入 JSON 控制命令，每行末尾必须有换行符：

```json
{"protocol":1,"command":"cancel","commandId":"01J..."}
{"protocol":1,"command":"status","commandId":"01J..."}
{"protocol":1,"command":"shutdown","commandId":"01J..."}
```

支持 stdin 控制的耗时一次性命令支持 `cancel`；`backend supervise` 另外支持
`status` 和 `shutdown`。具体能力以首个 `hello.capabilities` 为准。`commandId`
由调用方生成并保持唯一。关闭监督进程时，向其 stdin 发送 `shutdown`，不要直接
按进程名终止 Python。

## 输出与退出码

### 人类可读输出

默认 `--output human`，适合直接在 PowerShell 中查看。需要查看某条命令的完整
选项时使用：

```powershell
& $runtime --output human doctor --help
```

### NDJSON 输出

`--output ndjson` 适合 Electron 和自动化程序：

- stdout 每行恰好一个 JSON 对象；
- `hello` 是首事件，`result` 是终态事件；
- stdout 不混入普通文本、颜色控制字符或进度条；
- stderr 只用于 Runtime 诊断。

例如，把两条流分别保存：

```powershell
& $runtime --app-root $appRoot --output ndjson doctor 1> '.\doctor.ndjson' 2> '.\doctor.stderr.log'
$exitCode = $LASTEXITCODE
```

### 退出码

退出码是粗粒度分类；精确原因应读取 NDJSON `result.code`。

| 退出码 | 含义 |
| ---: | --- |
| `0` | 成功 |
| `2` | 参数错误 |
| `10` | 协议不兼容 |
| `20` | 前置条件不满足 |
| `30` | 网络或下载失败 |
| `40` | Git、仓库或目录替换失败 |
| `50` | uv、Python 或项目依赖失败 |
| `60` | 后端启动或运行失败 |
| `70` | 操作冲突或目录被锁定 |
| `130` | 用户取消 |

## 开发与验证

### 环境要求

- Windows 10/11；
- [PowerShell 7](https://learn.microsoft.com/powershell/)（`pwsh`）；
- Go 1.26 或更新版本；
- MSYS2 UCRT64 GCC/G++：仅在执行 CGO 或 race detector 时需要；
- `golangci-lint`：可选的额外静态检查工具。

### 标准验证

```powershell
$env:GOCACHE = Join-Path $env:TEMP 'auto-mas-runtime-verify'

$unformatted = & gofmt -l .
if ($LASTEXITCODE -ne 0) { throw 'gofmt failed' }
if ($unformatted) {
    $unformatted
    throw 'gofmt found unformatted files'
}

& go vet ./...
if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }

& go build -buildvcs=false ./...
if ($LASTEXITCODE -ne 0) { throw 'build failed' }

& go test ./... -count=1
if ($LASTEXITCODE -ne 0) { throw 'tests failed' }

& git diff --check
if ($LASTEXITCODE -ne 0) { throw 'diff check failed' }
```

执行 race detector 前，先把 GCC 所在目录放到当前 PowerShell 会话的 PATH 首位：

```powershell
$gccBin = Split-Path -Parent (Get-Command gcc -ErrorAction Stop).Source
$env:PATH = "$gccBin;$env:PATH"
$env:GOCACHE = Join-Path $env:TEMP 'auto-mas-runtime-race'

& go test -race ./... -count=1
if ($LASTEXITCODE -ne 0) { throw 'race tests failed' }
```

## 进一步阅读

- [文档导航](doc/README.md)
- [架构设计](doc/架构设计.md)：命令树、NDJSON、错误码、目录和生命周期契约
- [协议补充 v1](doc/契约补充-v1.md)：固定端口、身份校验和 development 边界
- [任务拆分](doc/任务拆分.md)：实现进度与任务验收项
- [代码审查清单](doc/代码审查清单.md)
- [AGENTS.md](AGENTS.md)：仓库开发规范

项目采用 [AGPL-3.0-or-later](LICENSE) 授权。
