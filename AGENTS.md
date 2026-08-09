# AGENTS.md

面向在本仓库工作的 AI Agent 和新加入的开发者。**动手前必读。**

本文件是“怎么在这个仓库干活”的入口；“系统应该是什么样”以 `doc/` 下的权威文档为准。
两者冲突时，以 `doc/` 为准，并按第 6.5 节流程先改文档。

---

## 1. 项目是什么

AUTO-MAS Runtime 用 Go 实现 **Windows 本机运行时管理程序**，产出单个可执行文件
`auto-mas-runtime.exe`，接管当前由 AUTO-MAS（Electron + Python）承担的初始化、更新
和后端进程管理职责。

Electron 通过「命令行参数数组 + stdin/stdout NDJSON + 退出码」调用 Runtime，
Vue 仍直接访问 Python 后端的 HTTP/WebSocket 业务接口。Runtime 只在控制面，
**不代理任何业务流量**。

**Runtime 负责：** 本机环境检测与部署、受管后端 Git 仓库更新、uv/Python/依赖同步、
后端进程启动与监督、健康检查、诊断、修复、清理、进程树回收。

**Runtime 不负责：** 业务 HTTP/WebSocket 代理、Python 插件依赖管理、自身自动更新、
Vue/Electron/Python 的改写、CI/CD 发布流程本身。

首版正式发布**仅支持 Windows**。2026-08-04 起登记两项**规划中**扩展（决策 D7/D8，
均不改变既有 Windows 契约）：跨平台适配（Linux 少数发行版 + macOS，Windows 仍为
首要平台，M11）与 PostHog 遥测（计划阶段，M12）；详见 `doc/架构设计.md`
「平台支持策略」「遥测策略」两节。

---

## 2. 当前状态（截至 2026-08-09）

| 里程碑 | 状态 |
| --- | --- |
| M0 工程基础与最小 CI | T0.1~T0.4 **已完成** |
| M1 协议层 `internal/protocol` | T1.0~T1.9 **已完成** |
| M2 基础设施（config/logging/state/lock/filesystem/mirror/下载器） | T2.1~T2.8 **已完成**（T2.8 为远端 CI 暴露的测试 8.3 短文件名假设，2026-08-04 修复） |
| M3 CLI 框架与基础命令 | T3.1~T3.8 **已完成**（含三轮对抗性审查的修复与可维护性收敛） |
| M4 工作区同步 | T4.0~T4.7 **已完成**（含三轮对抗性审查与组件矩阵） |
| M5 | T5.1~T5.8 **复审修复中**（`e67a548` 的完成结论已撤回，见 `doc/current/M5/`） |
| M6~M7、M9 | 未开始 |
| M10 工程可维护性收敛 | T10.1 文档信息架构已完成；后续阶段见维护设计 |
| M11 跨平台适配（Linux/macOS） | 规划中（决策 D7，2026-08-04 立项）；仅 T11.1 设计任务可执行 |
| M12 PostHog 遥测 | 规划中（决策 D8，2026-08-04 立项）；待 D-open-9 定稿，禁止提前实现 |

代码现状：

- `internal/protocol`、`config`、`filesystem`、`logging`、`state`、`lock`、`mirror`、
  `cli`、`doctor`、`cleanup`、`version` 已实现；
- `gitrepo` 已实现版本映射、go-git 浅克隆、仓库校验、原子替换、中断恢复和 workspace 服务；
  `uv` 已实现但正在修复 M5 复审问题；`backend`、`health`、`process` 仍只有占位；
- `internal/cli` 已按架构命令树注册全部命令，`version`/`doctor`/`cleanup`、workspace 和
  M5 environment/bootstrap/dependencies/repair 为真实实现；后续里程碑命令仍返回 `UNSUPPORTED_MODE`；
- `cmd/auto-mas-runtime/main.go` 是唯一持有 `os.Stdout` 的入口。

Git：远端 `origin` = `git@github.com:AUTO-MAS-Project/AUTO-MAS-Runtime.git`。
本地 `main` 与 `origin/main` 的实际关系以 `git status -sb` / `git log --oneline -5` 为准，
**不要依赖本文件里的哈希**（它会随每次提交过期）。
特性分支 `codex/runtime-implementation` 在 `.worktrees/codex-runtime-implementation` 下。

---

## 3. 权威文档（真实来源）

| 文档 | 内容 | 何时必读 |
| --- | --- | --- |
| [doc/README.md](doc/README.md) | 文档导航、分层与生命周期规则 | 查找任何项目文档时 |
| [doc/架构设计.md](doc/架构设计.md) | 冻结的系统架构：边界、CLI 命令树、NDJSON 协议、错误码/退出码/stage/state 全集、Git 更新流程、uv 策略、目录安全、测试矩阵、验收标准 | 任何涉及对外契约的改动 |
| [doc/契约补充-v1.md](doc/契约补充-v1.md) | 协议 v1 的 5 项定稿细节（C1~C5：固定端口 36163、身份注入环境变量、`failed` 字面量、`AUTO_MAS_SUPERVISED=1`、development 检查边界） | 涉及后端启动/健康检查/环境变量 |
| [doc/任务拆分.md](doc/任务拆分.md) | 逐任务清单、依赖、验收项、决策记录 D1~D6、待决项 D-open-*、AUTO-MAS 侧 TODO、变更记录 | **每次开工前**确认自己在做哪个任务 |
| [doc/代码审查清单.md](doc/代码审查清单.md) | 自动化门禁覆盖不到的架构边界检查 | 提交前自查、审查他人代码 |
| `doc/current/M*/` | 尚未完成任务的设计与实施计划 | 执行某个具体任务时 |
| `doc/archive/M*/` | 已完成阶段仍有解释价值的设计与审查记录 | 追溯设计背景时 |

**优先级：** 契约补充-v1（更具体） > 架构设计（概括） > 任务拆分（派生清单）。

---

## 4. 仓库结构与包边界

```text
cmd/auto-mas-runtime/  进程入口，唯一可持有 os.Stdout
internal/cli/          参数解析与应用服务分发（不含业务逻辑）
internal/protocol/     NDJSON 事件、错误码、生命周期状态机、renderer、stdin 控制
  └─ contracttest/     可复用的原始 NDJSON 契约断言库（后续命令一行注册接入）
internal/config/       配置与受管目录布局的唯一来源
internal/gitrepo/      版本 → 分支映射、浅克隆、受管仓库整体替换
internal/uv/           uv bootstrap、Python、主项目环境、后端启动命令
internal/backend/      后端启动/健康/单次重启/关闭编排
internal/process/      Windows Job Object 与子进程管道
internal/health/       后端健康与身份校验
internal/state/        跨进程持久化操作状态
internal/lock/         Windows 命名 Mutex 并发协调
internal/mirror/       四类下载源的轮换
internal/filesystem/   路径校验、原子移动、受控删除
internal/logging/      stderr 诊断与轮转操作日志
testdata/              组件与端到端测试夹具
doc/                   权威文档
```

**包边界纪律（来自架构设计「Go 项目结构」）：**

- `cli` 只解析参数、调用应用服务，不写业务逻辑；
- `protocol` 只定义契约，不感知具体命令；
- 路径拼接只能来自 `config`，其他包不得散落硬编码路径（T2.1 起生效）；
- uv 子进程只能经 `uv` 包的唯一执行器，其他包禁止自行 `exec.Command` 调 uv（T5.2 起生效）；
- **业务包不得直接向 stdout 写任何内容**，机器事件统一经 `protocol`。

---

## 5. 开发环境与命令

### 5.1 工具链

- Windows 10/11 + **PowerShell 7（`pwsh`）**；
- **Go 1.26**（`go.mod` 声明 `go 1.26`；本机当前为 Go 1.26.5）；
- MSYS2 UCRT64 **GCC/G++ 16.1.0**，`CGO_ENABLED=1`；
- golangci-lint 2.12.2+（可选，本机当前未安装）。

本机的 `go`、`gofmt`、`gcc`、`g++` 均可由 PowerShell 7 直接从 `PATH` 解析，命令和文档不得
固化用户目录下的工具绝对路径。运行 CGO 或 race detector 前，应把实际 GCC 目录提升到当前
会话的 `PATH` 首位，避免 PostgreSQL 等软件目录中的同名 `libwinpthread` / `zlib` DLL 被优先加载：

```powershell
$gccBin = Split-Path -Parent (Get-Command gcc -ErrorAction Stop).Source
$env:PATH = "$gccBin;$env:PATH"
```

### 5.2 标准验证门（提交前必须全绿）

```powershell
$env:GOCACHE = Join-Path $env:TEMP "auto-mas-runtime-verify"

$unformatted = & gofmt -l .
if ($LASTEXITCODE -ne 0) { throw "gofmt failed" }
if ($unformatted) { $unformatted; throw "gofmt found unformatted files" }
& go vet ./...                  ; if ($LASTEXITCODE -ne 0) { throw "go vet failed" }
& go build -buildvcs=false ./...; if ($LASTEXITCODE -ne 0) { throw "build failed" }
& go test ./... -count=1        ; if ($LASTEXITCODE -ne 0) { throw "tests failed" }
& git diff --check              ; if ($LASTEXITCODE -ne 0) { throw "diff check failed" }
```

并发相关改动追加重复跑：
`& go test ./internal/protocol -count=100; if ($LASTEXITCODE -ne 0) { throw "repeated tests failed" }`，
并执行 5.3 的 race detector。

**每个命令后必须检查 `$LASTEXITCODE`**：PowerShell 不会因原生命令失败而中断脚本，
漏检会导致“测试没跑却宣称通过”。

### 5.3 race detector 与已知限制

- 本机已具备 CGO 和 C 编译器；提升 GCC 目录优先级后执行完整 race 验证：

  ```powershell
  $gccBin = Split-Path -Parent (Get-Command gcc -ErrorAction Stop).Source
  $env:PATH = "$gccBin;$env:PATH"
  $env:GOCACHE = Join-Path $env:TEMP "auto-mas-runtime-race"
  & go test -race ./... -count=1
  if ($LASTEXITCODE -ne 0) { throw "race tests failed" }
  ```

- 当前 Codex 受管沙箱可能同时注入大小写不同的 `PATH` / `Path`，Go 重建 cgo 子进程环境时会使
  旧顺序重新生效，表象仅为 `runtime/cgo: ... cgo.exe: exit status 2`。确认 GCC 路径与 DLL
  优先级正确后，应获批在沙箱外重跑同一命令；只有退出码为 0 的完整输出才能作为 race 通过证据；
- `golangci-lint` 本机未安装，只在需要时按 README 安装固定版本。

### 5.4 CI

`.github/workflows/ci.yml`：push / PR 触发，`windows-latest` + `pwsh`，
步骤 = checkout → setup-go（读 `go.mod`）→ gofmt 检查 → `go vet` → `go build` → `go test`。
本地基础验证门与 CI 保持一致；race detector 是并发相关改动的本地追加门，当前 CI 未覆盖，
不得把本机 race 结果表述成 CI 证据。

---

## 6. 工作流程

### 6.1 任务驱动，不自由发挥

所有实现工作必须对应 `doc/任务拆分.md` 中的一个任务编号（`T<里程碑>.<序号>`）。
开工前确认：任务的「依赖」是否已完成、「内容」和「验收」写了什么。
清单里没有的需求，先补进任务拆分并登记变更记录。

### 6.2 四段式（本仓库既有实践）

```text
设计 doc/current/M*/设计-T*.md  →  计划 doc/current/M*/计划-T*.md  →  TDD 逐任务实现  →  审查 + 验收回写
```

1. **设计**：目标、范围与边界（含明确的「不负责」）、API 形态、并发/失败语义；
2. **计划**：拆成若干可独立提交的 Task，每个 Task 写明「文件 / 红灯 / 绿灯 / 验证与提交」，
   固定测试函数名，验证命令直接可粘贴执行；计划不复制大段最终代码，任务完成后删除并由 Git 历史追溯；
3. **实现**：**严格 TDD**——先写失败测试并确认失败原因正确，再写最小实现；
4. **审查**：每个 Task 提交后独立审查，Critical/Important 修完才进入下一个 Task；
   修复独立提交（`fix: ...`）。

### 6.3 完成定义（DoD，逐条满足才算完成）

1. 代码已提交到 `main`（或经 PR 合入）；
2. `gofmt` 无 diff、`go vet ./...` 无告警、`go test ./...` 全绿；
3. 任务条目中的「验收」逐条满足；
4. 不回退任何已通过的协议契约测试；
5. 涉及对外契约的实现与 `doc/架构设计.md` 一致。

### 6.4 完成后回写进度

在 `doc/任务拆分.md` 中：`- [ ]` 改 `- [x]`，标题行尾追加 `✅ YYYY-MM-DD <commit 短哈希>`；
进行中用 `🚧`，受阻用 `⛔ <一句话原因>` 并登记第 7 章「变更记录」；
里程碑复选框只有在其下**全部任务**勾选后才能勾。
进度提交独立于代码提交，形如 `docs: 记录 M1 协议层完成情况`。

### 6.5 发现文档与实现矛盾时

**先改文档 + 登记变更记录，再写代码。** 不允许用代码“事实上”推翻已冻结的契约。

### 6.6 分支与 worktree

特性开发在 `.worktrees/<name>` 下的 worktree 里进行（`.worktrees/` 已被 `.gitignore` 忽略），
完成后回 AUTO-MAS Runtime 主 worktree 根目录执行 `git merge --ff-only <branch>`，
并确认 `git rev-list --left-right --count main...<branch>` 为 `0 0`。

### 6.7 🚩 推送与外发需要明确授权

`git push`、创建远端仓库/Release、向任何外部服务发送仓库内容，
**必须先获得用户明确授权**，每次都要单独授权，不因上次批准过就顺手推一下。
授权范围要说清楚：`git push` 推的是分支 tip，会连同全部未推送的祖先提交一起发布，
先用 `git rev-list --left-right --count origin/main...HEAD` 确认实际数量再请求授权。

---

## 7. 提交规范

- Conventional Commits，**type 使用英文小写，描述使用中文**，标题结尾带任务号：

  ```text
  feat: 强制协议终态 result (T1.6)
  fix: 限制重复键诊断信息规模 (T1.6)
  test: 覆盖协议终态契约 (T1.6)
  docs: 记录 M1 协议层完成情况
  ```

- 常用类型：`feat` / `fix` / `test` / `docs` / `refactor` / `chore` / `ci`；
- 一个 Task 一个提交，只 `git add` 该 Task 明确列出的文件（计划文档里会显式校验暂存清单）；
- 提交前跑 `git diff --check`；不要 `--no-verify`，不要跳过签名。

---

## 8. 代码规范

### 8.1 语言与注释

| 对象 | 语言 |
| --- | --- |
| 标识符、包名、文件名 | 英文 |
| **注释（含包注释、导出声明的 doc comment、实现内注释）** | **中文** |
| Go error 字符串（`errors.New` / `fmt.Errorf`） | 英文、小写开头、无结尾标点 |
| 协议中用户可见的 `message` 字段 | 中文（如「正在安装 Python 依赖」） |
| 提交信息 | 英文 type + 中文描述 |
| `doc/` 下的设计与任务文档 | 中文，中英文之间留空格 |

**中文注释仍遵循 Go 官方 doc comment 格式**（[Go Doc Comments](https://go.dev/doc/comment)）：
以被注释的标识符开头，标识符保持英文原样，随后用中文说明；每个包必须有包注释或 `doc.go`。

```go
// Package protocol 定义单次 Runtime 操作的事件、错误、生命周期、渲染和 stdin 控制契约。
package protocol

// Emitter 通过 ProcessOutput 写出类型化协议事件。
type Emitter struct{ ... }

// NewProcessOutput 创建进程内唯一的 NDJSON 输出所有者。
// output 一旦交出即由 ProcessOutput 独占，调用方不得再直接写入。
func NewProcessOutput(output io.Writer) (*ProcessOutput, error) { ... }
```

注释写「为什么」，不复述「做了什么」；能用命名表达的不写注释。

**存量代码：** `internal/protocol` 现有注释为英文，**不做批量重写**（避免无意义 diff 掩盖真实改动）。
新增代码和被修改的声明改用中文，但**同一函数/同一声明内不混排两种语言**。
需要统一迁移时作为独立任务执行，单独提交。

### 8.2 遵循的公开标准

本仓库采用 Go 社区与大厂的公开标准，不自造风格。冲突时按以下优先级：

1. [Effective Go](https://go.dev/doc/effective_go) 与 [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) —— 语言级惯例，最高优先级；
2. [Google Go Style Guide](https://google.github.io/styleguide/go/)（Style Guide / Style Decisions / Best Practices）—— 大型代码库的一致性决策；
3. [Uber Go Style Guide](https://github.com/uber-go/guide/blob/master/style.md) —— 具体条目（并发、性能、错误）可作补充参考；
4. 本文件 8.9~8.13 的项目特有约束 —— **覆盖以上通用规则**。

工具层面由 `gofmt` + `goimports` + `go vet` + `.golangci.yml`
（`errorlint`、`gocritic`、`misspell`、`nilerr`、`nolintlint`、`revive`）强制，
风格问题优先靠工具解决，不靠人工争论。

### 8.3 命名

- 包名：小写单字，不用下划线、驼峰或复数；禁止 `util`、`common`、`base`、`helper` 等无信息量的包名；
- 避免口吃：`protocol.Event` 而非 `protocol.ProtocolEvent`；`config.Layout` 而非 `config.ConfigLayout`；
- 缩写整体大小写一致：`ID`、`URL`、`HTTP`、`JSON`、`PID`；`operationID` 而非 `operationId`
  （JSON 字段名 `operationId` 由 struct tag 表达，不影响 Go 字段名）；
- 单方法接口用 `-er` 后缀：`EventRenderer`、`Downloader`；
- getter 不加 `Get` 前缀：`o.Sequence()` 而非 `o.GetSequence()`；
- 变量名长度与作用域成正比：循环内 `i`、`err` 可以，包级导出变量必须自解释；
- receiver 用 1~2 个字母且同一类型全程一致（`func (o *ProcessOutput)` 就一直是 `o`）；不用 `this`/`self`；
- 导出的 sentinel error 用 `Err` 前缀：`ErrResultAlreadyEmitted`；错误类型用 `Error` 后缀。

### 8.4 错误处理

- **不忽略错误。** 确实要忽略时写成 `_ = f()` 并在同行/上一行用注释说明为什么安全；
- 包装保留错误链：`fmt.Errorf("clone %s: %w", branch, err)`；判定一律用 `errors.Is` / `errors.As`，
  **禁止 `err == ErrX` 和字符串匹配**（`errorlint` 会拦）；
- 错误字符串英文、小写开头、无结尾标点、不含换行；不要以 "failed to" 开头堆叠，
  包装时只加本层上下文；
- 每层只包装一次，不重复堆同样的信息；
- happy path 左对齐：失败尽早 `return`，减少 `else` 与嵌套；
- **库代码不 `panic`、不 `os.Exit`、不 `log.Fatal`**；只有 `cmd/auto-mas-runtime` 的 `main` 可以决定退出码；
- `panic` 只用于表示程序自身的不变量被破坏（且必须有测试覆盖该不变量）；
- 错误必须能映射到架构文档的错误码全集，见 8.11。

### 8.5 context 与并发

- `ctx context.Context` 永远是第一个参数，命名固定为 `ctx`；**不把 ctx 存进结构体**；
- 不传 `nil` ctx；测试用 `t.Context()` 或 `context.Background()`；
- 超时与取消必须贯通到底层：HTTP 请求、子进程（`exec.CommandContext`）、文件轮询都要接 ctx；
- 谁启动 goroutine 谁负责它的退出路径，不留泄漏；退出条件写进注释；
- `sync.Mutex` 用零值、**不用指针**；mutex 紧邻它保护的字段声明，并注释保护范围
  （`internal/protocol.ProcessOutput` 是现成范例：单一线性化域覆盖 renderer、sequence、terminal、warning ledger）；
- 复制含 mutex 的结构体是 bug；需要传递就用指针；
- **禁止用 `time.Sleep` 做同步**（生产和测试都是）；用 channel、`sync.WaitGroup` 或 barrier。

### 8.6 接口、依赖注入与可测试性

- **接受接口、返回具体类型**；
- 接口在**消费方**定义，不在实现方；保持小（1~3 个方法）；
- 不做投机抽象：只有真的存在第二个实现或测试替身需求时才抽接口；
- 架构文档强制注入的依赖：网络、Git、uv、进程、时钟。这些必须以接口或函数字段注入，
  单元测试不得触碰真实公网、真实 Python/Git（见第 9 节）；
- 时钟注入沿用现有形态：`WithClock(func() time.Time)`。

### 8.7 包组织、导入与文件结构

- import 分三组，组间空行，由 `goimports` 维护：标准库 / 第三方 / `github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/...`；
- 禁止点导入（`import . "x"`）；`_` 空白导入必须写明用途注释；
- **避免 `init()` 和包级可变全局状态**；配置显式传参；确需包级常量用 `const`；
- 文件内顺序：包注释 → import → 常量 → 类型 → 构造函数 → 方法 → 包内辅助函数；
- 一个文件聚焦一个主题（现有 `emitter.go` / `renderer.go` / `lifecycle.go` / `control.go` 即此风格）；
- 测试文件与被测文件同名加 `_test.go`；需要白盒断言时用同包测试，
  对外 API 用 `_test` 外部包（现有 `renderer_internal_test.go` 与 `renderer_test.go` 的分工）。

### 8.8 其他惯例

- 零值可用：`var buf bytes.Buffer` 而非 `new(bytes.Buffer)`；设计类型时优先让零值有意义；
- 已知容量就预分配：`make([]T, 0, n)`、`make(map[K]V, n)`；
- 字符串拼接用 `strings.Builder`，不在循环里 `+=`；
- 结构体字面量必须写字段名；
- `defer` 用于释放资源，注意**不要在循环体内 defer**；
- 时间用 `time.Duration`，不用裸 `int` 秒；含单位的字段名带单位（`timeoutSeconds` 只在 JSON 契约里出现）；
- 长函数禁止裸返回（naked return）；
- 枚举用具名类型 + 常量组 + `String()`，并提供合法性校验（现有 stage / status 即此模式）；
- 不引入非必要依赖：当前生产依赖只有 `golang.org/x/sys/windows`；新增依赖需在任务文档中说明理由。
  已批准方向：CLI 使用 Cobra、Git 使用 go-git、Windows 系统调用继续使用固定版本 `x/sys/windows`；
  `google/go-cmp` 仅可用于测试结构比较。具体边界见 `doc/maintenance/现成库替换评估.md`。

### 8.9 stdout 所有权（最容易违反，务必逐条对照 `doc/代码审查清单.md`）

- 只有 `cmd/auto-mas-runtime` 进程入口可以持有 `os.Stdout`；`internal` 业务包不得导入或保存它；
- `--output ndjson` 时进程入口只创建一个 `protocol.ProcessOutput` 和唯一的 `protocol.Emitter`
  （`ProcessOutput` 会拒绝第二个 emitter）；业务包只接收类型化事件出口；
- 七种事件（`hello`/`progress`/`state`/`log`/`warning`/`error`/`result`）全部经同一个 Emitter 发射，
  不得直接 `fmt.Print*`、`log.Print*` 或 `json.NewEncoder` 写 stdout；
- Runtime 自身诊断写 stderr；受管进程的 stdout/stderr 转成 `log` 事件 + 轮转日志文件；
- 新增输出路径时执行并逐项审查：

  ```powershell
  rg -n 'os\.Stdout|fmt\.F?[Pp]rint|log\.[Pp]rint|json\.NewEncoder' --glob '*.go' .
  if ($LASTEXITCODE -gt 1) { throw "stdout ownership scan failed" }
  ```

### 8.10 协议不变量

首事件必为 `hello`；`sequence` 进程内从 1 严格递增；每行恰好一个 JSON object 且每行 flush；
`result` 恰好一次且最后，发出后拒绝一切后续事件；warning 由协议层权威汇总进
`result.details.warnings`（上限 256，超出置 `truncated`）。

### 8.11 错误与契约

- 错误码、退出码、`retryable`、`remediation` 四元组以架构文档「错误码全集」为唯一来源，
  失败 `result` 必须复述主错误的四元组；
- 新增错误码可在协议 v1 内追加；**删除、改名或改变既有语义必须升级协议版本**；
- `stage` 标识可追加，既有标识不得改名；
- 调用方（Electron/测试）只按稳定字段判断，**禁止解析中文文案、uv 输出或 Git 输出驱动流程**——
  实现侧同样不许靠解析自然语言做业务分支。

### 8.12 Windows 与路径安全

- 任何递归删除前必须：规范化绝对路径 → 校验位于受管根内 → 排除安装根/用户数据根/盘符根/空路径 →
  拒绝跟随未知 Junction/符号链接 → 按状态确认目录身份 → 写审计日志 → **身份不明即失败关闭，绝不猜测**；
- 用户数据（`config/ data/ history/ script/ debug/ plugins/`）、插件运行包和外部脚本目录
  **任何自动流程都不得删除或移动**；
- 禁止按进程名批量终止（`taskkill /im python.exe` 之类），进程树只能通过自己创建的 Job Object 回收。

### 8.13 外部进程

- 一律 `exec.CommandContext` + 参数数组，**不拼 Shell 字符串**；
- 禁止直接调用 `python.exe` / `pip` / `.venv\Scripts\python.exe`，Python 工具链统一走受管 `uv.exe`；
- 禁止 `uv self update`、在线安装脚本（`irm astral.sh/uv/install.ps1`）；
- TLS 校验默认开启且不提供关闭开关。

---

## 9. 测试规范

- 分层：单元 / 组件 / 契约 / Windows E2E。必测场景见架构设计「自动化测试矩阵」，
  实现某任务时对照该表逐行覆盖；
- 网络、Git、uv、进程、时钟必须经接口注入；单元测试**不得依赖真实公网或用户机器上的 Python/Git**；
- 夹具：本地裸仓库 + 本地 HTTP 服务（Git）、假 `uv.exe`（T5.8）、假后端进程（T6.6）；
- 所有测试使用临时受管根目录，结束后校验无残留进程、Mutex、临时目录；
- 并发/线性化用 barrier + 超时做**确定性**证明，不靠 `time.Sleep` 撞运气，并用 `-count=100` 复跑；
- 新命令接入契约测试的成本应保持为**一行注册**（`contracttest.Register`）。

Go 测试惯例（[Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) /
[Google Best Practices](https://google.github.io/styleguide/go/best-practices)）：

- **表驱动测试 + `t.Run` 子测试**，用例结构体带 `name` 字段；
- 测试辅助函数第一行写 `t.Helper()`，失败行号才会指向调用处；
- 临时目录用 `t.TempDir()`，清理用 `t.Cleanup()`，不手写 `defer os.RemoveAll`；
- 普通条件、错误链、顺序和副作用使用标准库显式断言；结构体、slice、map 的语义比较可使用
  仅测试依赖 `google/go-cmp`；不引入 testify/gomega，不建立通用 `assert` 包；
- 失败信息写成 `got X, want Y` 形式，带上定位所需的输入；
- 测试函数命名沿用仓库惯例 `TestType_Scenario`（如 `TestEmitter_ConcurrentResults`）；
  计划文档中固定的测试函数名不得随意改动；
- 用 `-run` 跑定向测试时必须同时确认「退出码为 0」「输出不含 `[no tests to run]`」
  「出现 `--- PASS:`」三件事，见 11 节。

---

## 10. 红线清单（违反即回退）

1. 业务包直接写 stdout，或绕过 `protocol.Emitter` 发协议事件；
2. 改动已冻结的对外契约（字段、错误码、stage、state、环境变量、目录布局）却没先改文档；
3. 未获授权执行 `git push` 或把仓库内容发往外部服务；
4. 直接调用 `python`/`pip`，或让 Runtime 去管插件依赖；
5. 按进程名批量杀进程；
6. 自动流程删除用户数据、诊断日志、插件目录或受管根之外的路径；
7. 先删旧 `repo` 再下载新版本（必须先克隆校验完成再整体替换）；
8. 靠解析自然语言输出判断业务状态；
9. 宣称“测试通过 / race 通过”却没有当次完整命令和退出码证据（工具可用不等于测试通过）。

---

## 11. 常见陷阱

- **工具从 PATH 解析**：不要固化本机绝对路径；顺手把 `$env:GOCACHE` 指到临时目录，
  避免权限问题和工作区污染。
- **GCC 能找到但 cgo 失败**：先按 5.1 把 UCRT64 `gcc.exe` 所在目录提升到 `PATH` 首位，
  排除同名 DLL 冲突；Codex 沙箱内仍失败则按 5.3 获批在沙箱外复跑。
- **`$LASTEXITCODE` 不检查等于没跑测试**：PowerShell 不会自动中断。
- **`-run` 打错正则 → `[no tests to run]` 却退出码 0**：计划文档里的模板会同时校验
  “退出码为 0”“输出不含 `[no tests to run]`”“出现 `--- PASS:`”，照抄这个模式。
- **注释中文、标识符英文**：注释（含 doc comment）写中文，doc comment 仍以英文标识符开头；
  Go error 字符串保持英文小写；不要把设计文档写成英文，也不要在同一声明里中英混排。
- **改协议前先看 `doc/契约补充-v1.md`**：架构设计里的概括描述常被它进一步收紧。
- **决策已冻结的事项不要重开**：D1~D6 与 C1~C5 是用户已确认的结论；D-open-4~7 才是待决项。
