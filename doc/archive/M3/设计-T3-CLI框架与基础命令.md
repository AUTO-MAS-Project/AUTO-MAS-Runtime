# M3 CLI 框架与基础命令设计

状态：设计中（2026-08-02）。

范围：`T3.1`（CLI 框架与全局参数）、`T3.2`（version）、`T3.3`（doctor）、
`T3.4`（cleanup）。权威依据：`doc/架构设计.md`（CLI 设计、NDJSON 公共结构、
错误码全集、退出码、目录安全、Go 项目结构、测试矩阵）、`doc/契约补充-v1.md`
（C1~C5）、`doc/任务拆分.md`（M3、M10、变更记录）、`doc/代码审查清单.md`、
`doc/maintenance/现成库替换评估.md`。

## 1. 目标与边界

目标：把当前 `internal/cli` 的占位骨架替换为可运行的 CLI 框架，完成
`version`、`doctor`、`cleanup` 三个真实命令，其余命令按冻结命令树注册为
稳定「未实现」骨架；所有命令统一走「hello → 执行 → result」协议生命周期；
human 与 ndjson 双输出模式全部经 `protocol.ProcessOutput` 投影。

本阶段不负责：

- 不实现 M4/M5/M6 业务（workspace sync、environment、dependencies、backend、
  bootstrap、repair 的真实逻辑）；
- 不迁移或重构 M1/M2 既有包（protocol/config/state/lock/filesystem/mirror/
  logging/downloader）；
- 不引入 Viper、testify、gomega 或通用 assert 包；不实施 T10.2/T10.3；
- 不改变冻结的 NDJSON 字段、错误码、退出码、stage、state、目录布局与环境变量；
- 不接入 stdin 控制通道（`stdin.cancel` 能力留给 M4+ 需要取消的命令接线）。

## 2. 总体决策

### D1 Cobra 固定版本

采用 Cobra `v1.10.2`（2025-12-03 发布，2026-08-02 经 `proxy.golang.org`
`/@latest` 确认为当前稳定版）。只采用 Cobra，不引入 Viper。Cobra 类型只存在于
`internal/cli`，不得扩散到 config/protocol/state/lock/filesystem 等业务包。

### D2 cli 入口形态与 I/O 注入

`internal/cli` 提供进程入口可调用的唯一 API：

```go
type IO struct {
    In  io.Reader
    Out io.Writer
    Err io.Writer
}

func Execute(ctx context.Context, args []string, io IO, options ...Option) int
```

- `cmd/auto-mas-runtime/main.go` 仍是唯一持有 `os.Stdout` / `os.Stderr` 的入口，
  只负责把真实 I/O 与 `os.Args[1:]` 交给 `cli.Execute`，并把返回的退出码交给
  `os.Exit`；不在入口之外调用 `os.Exit` / `log.Fatal` / `panic`。
- `internal` 全部包不得导入或保存 `os.Stdout` / `os.Stderr`；stdin、stdout、
  stderr 一律显式注入。
- `Execute` 每次调用独立构建命令树，不存在跨调用共享的可变状态；
  `Option` 仅用于注入 cwd、时钟、version 来源和服务工厂（测试替身点），
  生产入口不传任何 Option。

### D3 新增应用服务包

按「cli 只解析参数并调用应用服务」的边界，本阶段新增三个内部包：

- `internal/version`：版本/构建信息注入点（T7.1 的占位实现，T3.2 使用）；
- `internal/doctor`：doctor 只读检查服务；
- `internal/cleanup`：cleanup 清理服务。

这些包只组合使用现有 M2 能力（config/state/lock/filesystem/protocol），
不复制 M2 内部实现；包边界在 `doc/任务拆分.md` 变更记录中登记。

### D4 未实现命令的错误映射

已注册但未实现的命令统一返回冻结错误码 `UNSUPPORTED_MODE`
（退出码 2、`retryable=false`、remediation=`update-desktop`），
中文 message 为「命令尚未实现」；不 panic、不静默成功、不输出临时调试文本。
不新增错误码：`UNSUPPORTED_MODE` 是文档错误码全集中与「不支持」语义最接近的
冻结值，避免为骨架命令扩大协议面。

每个骨架命令使用冻结 stage 集中最接近其领域的一个 stage（见命令树表），
保证 error/result 的 `stage` 通过契约测试的冻结全集断言。

## 3. 命令树与骨架映射

```text
auto-mas-runtime
├── version            （T3.2 实现）
├── doctor             （T3.3 实现）
├── bootstrap          （骨架，--version；stage=bootstrap）
├── workspace          （组命令）
│   ├── check          （骨架；stage=workspace.check）
│   └── sync --version （骨架；stage=workspace.swap）
├── environment        （组命令）
│   ├── check          （骨架；stage=uv.check）
│   ├── ensure         （骨架；stage=uv.check）
│   └── repair         （骨架；stage=uv.verify）
├── dependencies       （组命令）
│   ├── check          （骨架；stage=dependencies.check）
│   ├── sync           （骨架；stage=dependencies.sync）
│   └── rebuild        （骨架；stage=dependencies.rebuild）
├── backend
│   └── supervise      （骨架；stage=backend.spawn）
├── repair             （骨架；stage=repair）
└── cleanup            （T3.4 实现）
```

规则：

1. 所有子命令（含骨架）出现在 help 中，与架构文档命令树一致；
2. Cobra 自动生成的 `completion` 命令通过
   `CompletionOptions.DisableDefaultCmd = true` 关闭；默认 `help` 命令隐藏，
   不进入帮助清单；
3. 骨架命令按架构文档注册其专属 flag（如 `bootstrap --version`、
   `workspace sync --version`），flag 校验留给实现该命令的任务；
4. 组命令（workspace/environment/dependencies/backend）自身不可执行，
   调用组命令时输出 help 并以 0 退出（Cobra 默认行为）。

## 4. cli / main / protocol / 应用服务边界

```text
cmd/auto-mas-runtime/main.go
    │ os.Stdin / os.Stdout / os.Stderr
    ▼
internal/cli.Execute(ctx, args, IO)
    │ 构建 Cobra 命令树；解析并校验全局参数
    ├─ 解析失败 → stderr 诊断 → 退出码 2（不承诺 hello/result）
    ├─ --protocol 不兼容 → stderr 诊断 → 退出码 10（不承诺 hello/result）
    ├─ --help → 按输出模式路由帮助文本 → 退出码 0
    └─ 解析成功 → protocol.ProcessOutput(+renderer)
         └─ protocol.Emitter（唯一）→ 应用服务
              ├─ internal/version（T3.2）
              ├─ internal/doctor（T3.3）
              └─ internal/cleanup（T3.4）
```

职责划分：

- `cli`：命令树、参数解析与校验、输出模式选择、协议会话编排、错误/退出码
  顶层映射；不写业务逻辑，不复制 M2 能力；
- 应用服务：只接收 `*protocol.Emitter` 类型化事件出口，返回业务报告或携带
  冻结错误码的错误；不接触 stdout writer；
- `protocol`：维持现状，不因 M3 修改任何事件/错误/值定义；
- `main`：只注入 I/O 并决定最终退出码。

## 5. 输出模式与事件投影

`--output` 取值 `human`（默认）或 `ndjson`：

- `ndjson`：`protocol.NewProcessOutput(io.Out)`，NDJSON renderer；
- `human`：`protocol.NewHumanRenderer(io.Out, io.Err)` +
  `NewProcessOutputWithRenderer`，事件由 human renderer 投影；
- 两种模式共用同一套业务事件流与生命周期，差异只发生在 renderer；
- 机器语义（hello 首发、sequence 递增、result 唯一且最后）对两种模式
  都由 ProcessOutput 保证。

本阶段所有命令 `hello.capabilities` 为空数组 `[]`：M3 命令不接受 stdin
控制命令、不发射生命周期 state、不转发受管进程日志。

## 6. 解析失败与执行失败的生命周期

| 场景 | 输出 | 退出码 |
| --- | --- | --- |
| 未知 flag / 缺少参数 / 非法 output / 非法 mirror / offline 冲突 | stderr 诊断 | 2 |
| `--app-root` 无法构造 Layout | stderr 诊断 | 2 |
| `--protocol` 非整数 | stderr 诊断 | 2 |
| `--protocol` ≠ 1（不兼容） | stderr 诊断 | 10 |
| `--help` / 无子命令 | 帮助文本（human→stdout，ndjson→stderr） | 0 |
| 解析成功 | hello → 事件 → result | 按 result.code 的退出码 |

解析失败路径不创建 `ProcessOutput`、不发射 hello/result；诊断写 stderr，
文本为英文错误串（符合 Go error 规范），前缀 `auto-mas-runtime: `。

`--protocol` 属于「解析成功前」的语义校验：调用方与 Runtime 协议不一致时，
发出 v1 事件没有意义，因此与解析失败同一通道（stderr + 固定退出码 10），
不承诺 hello/result。该决定写入本设计与任务拆分变更记录。

## 7. hello/result 统一执行框架

会话框架（位于 cli 内部，所有命令共用）：

1. 参数解析与全局校验全部通过；
2. 按输出模式创建 `ProcessOutput`；
3. `NewEmitter(version.Version, 命令路径, []capabilities{})` 发射 hello；
4. 调用命令对应的应用服务（上下文为 `Execute` 传入的 ctx）；
5. 服务成功：`NewSuccessResult(stage, "succeeded", message, details)` 发射
   result，退出码 0；
6. 服务失败：先把错误映射为 `protocol.ErrorEvent`
   （code/stage/message/retryable/remediation 四元组来自冻结错误定义），
   发射 error，再用 `NewFailureResult` 复述四元组发射 result，
   退出码取 `LookupErrorDefinition(code).ExitCode`；
7. `context.Canceled` 映射为 `OPERATION_CANCELLED`（退出码 130）；
8. 事件发射失败（write error）时不再尝试协议输出，诊断写 stderr，
   退出码取主错误定义；若主错误不可映射则取 `OUTPUT_WRITE_FAILED`（20）。

服务返回的错误通过消费方定义的窄接口提供映射所需字段：

```go
type operationError interface {
    error
    Code() protocol.Code
    Stage() protocol.Stage
    Message() string
    Details() map[string]any
}
```

接口在 `internal/cli` 定义（消费方），doctor/cleanup 包各自实现自己的
错误类型，不导入 cli。

## 8. Cobra 行为固定

1. 根命令设置 `SilenceErrors=true`、`SilenceUsage=true`，保证 Cobra 不向
   stdout/stderr 打印 usage 或错误；
2. 根命令 `SetOut`/`SetErr`：human 模式 `SetOut(io.Out)`、`SetErr(io.Err)`；
   ndjson 模式 `SetOut(io.Err)`、`SetErr(io.Err)`——帮助与任何内部文本
   永不进入 NDJSON stdout；
3. 解析错误由 cli 统一以 `auto-mas-runtime: <err>` 写 stderr；
4. `--help`/`-h` 由 Cobra 内建触发，跳过会话框架，退出码 0；
5. 关闭自动 completion；隐藏默认 help 命令；
6. 帮助输出结构使用 Cobra 默认模板，但命令清单只含架构命令树；
7. `Execute` 每次创建独立根命令，测试可并行调用。

## 9. 全局参数与校验规则

全局参数注册在根命令（persistent flags），全部子命令继承：

| flag | 类型 | 规则 |
| --- | --- | --- |
| `--app-root` | string | 默认当前工作目录；经 `config.NewLayout(value, cwd)` 构造，失败即解析错误（退出码 2） |
| `--output` | string | `human`（默认）或 `ndjson`；其他值解析错误 |
| `--protocol` | string | 解析为整数；非整数解析错误；不等于 `protocol.Version`（1）→ 退出码 10 |
| `--mirror <kind>=<key>` | string slice，可重复 | 按出现顺序收集；kind 必须属于 `mirror.AllKinds()`；同一 kind 重复出现 → 解析错误 |
| `--offline` | bool | 与任何 `--mirror` 或 `--mirror-only` 互斥 |
| `--mirror-only` | bool | 与 `--offline` 互斥 |

镜像解析规则：

- 解析结果构造 `mirror.PolicySpec{Preferred: map[Kind]string, Offline, MirrorOnly}`，
  再经 `mirror.NewPolicy` 统一校验 kind/key/互斥（复用 M2 模型，不在 cli
  建第二套）；`NewPolicy` 失败一律映射为解析错误（退出码 2）；
- `--mirror` 的顺序对同一 kind 无意义（每 kind 只允许一个首选 key），
  因此重复 kind 直接拒绝，避免静默覆盖；
- 解析后的 `mirror.Policy` 存入本次执行上下文，M4+ 命令消费；本阶段
  version/doctor/cleanup 不联网，只解析并持有该值。

## 10. version 构建信息注入边界

`internal/version` 提供：

```go
var (
    Version   = "dev"   // -ldflags -X ... 注入
    Commit    = ""      // -ldflags 注入，可为空
    BuildDate = ""      // -ldflags 注入，可为空
)

type Info struct {
    Version   string
    Protocol  int
    Commit    string
    BuildDate string
    GoVersion string
}

func Load(ctx context.Context) (Info, error)
```

规则：

- 未注入时 `Version="dev"`、`Commit=""`、`BuildDate=""`，不伪造发布版本，
  不读取当前目录/Git 信息；
- 变量只在构建期通过 ldflags 写入，运行期只读；测试通过 cli Option 注入
  `Load` 替身，不修改包级变量，避免全局可变状态竞态；
- `hello.runtimeVersion` 与 version 命令消费同一 `Info.Version`；
- 源错误兜底：若 `Load` 返回的错误未实现 cli 的 operationError 窄接口，
  version 命令把它映射为 `OUTPUT_WRITE_FAILED`（消息「无法获取版本信息」）。
  生产路径的变量与 `runtime.Version()` 不会失败，该映射只作为稳定防御收口，
  同时为契约测试的 failure 终态提供确定性入口；
- 正式 ldflags 接线属于 T7.1，本阶段只建立注入点与稳定占位行为。

## 11. doctor 只读检查模型

服务包 `internal/doctor`，`Run(ctx, emitter) (Report, error)`。检查项固定：

| ID | 检查 | 判定 |
| --- | --- | --- |
| `app-root` | app-root 存在且为目录 | ok / missing |
| `layout` | repo、runtime-state、runtime、logs 存在性 | 逐目录 ok / missing |
| `uv` | `runtime/tools/uv/<ver>/uv.exe` 存在并执行 `--version` | ok / missing / error |
| `python` | `runtime/environment/python` 存在且含 `python.exe` | ok / missing |
| `repo` | `repo/` 存在；`repo/res/version.json` 存在且可解析 | ok / missing / error |
| `venv` | `runtime/environment/venv` 存在 | ok / missing |
| `runtime-state` | 四个状态文件存在、大小有界、JSON 可解析 | 逐文件 ok / missing / error |
| `mutex` | `lock.Set` 零等待探测 backend/mutation | ok / error（探测故障） |
| `disk` | 磁盘剩余空间探测 | ok / error |

结构：

```go
type Status string // "ok" | "missing" | "error"（doctor 自有稳定值，非协议枚举）
type Check struct {
    ID      string
    Name    string
    Status  Status
    Message string
    Details map[string]any
}
type Report struct {
    Checks  []Check
    Summary Summary
}
```

注入式探针（消费方定义最小接口/函数字段）：

```go
type Probes struct {
    UVVersion func(ctx context.Context, exePath string) (string, error)
    DiskFree  func(ctx context.Context, path string) (uint64, error)
}
```

生产实现：uv 探测用 `exec.CommandContext(exe, "--version")`（只读；uv 包
正式执行器 T5.2 落地后改经 uv 包）；磁盘探测用 Windows API。测试注入假函数，
不触碰真实公网、真实 Python 或用户机器 Git。

只读不变量：

- doctor 不获取 mutation/backend 锁，只用 `lock.Set.Probe` 零等待探测；
- doctor 不创建/修改/删除任何文件；`runtime-state` 一致性检查只读原始文件
  并做结构解析，**不使用 `state.Store`**（StateFiles Read 首次会建立持久
  guard `G`，违反无副作用约束；深层一致性验证属 T2.3/T4.5 职责）；
- 单项异常不提前终止：所有检查都执行并汇总，检查失败只影响该项状态；
- 即使存在 error 检查项，doctor 命令仍成功（`success=true`），
  完整检查列表在 `result.details.checks`；
- 每个检查发射一条 progress 事件（running → succeeded/failed），
  human 模式由此获得可读逐项输出。

## 12. cleanup 设计与安全语义

服务包 `internal/cleanup`，`Run(ctx, emitter) (Report, error)`。

### 12.1 锁

1. `lock.NewSet(ctx, layout)` 后 `AcquireMutation(ctx)` 零等待获取 mutation
   Mutex；冲突保持既有错误码：`MUTATION_IN_PROGRESS`、
   `BACKEND_STILL_RUNNING`（peer backend 占用）、`MUTEX_OPERATION_FAILED`；
2. 获取成功后 `defer lease.Close()` + `set.Close()`；
3. 获取前 ctx 已取消 → `OPERATION_CANCELLED`；
4. 获取后处理期间 ctx 取消 → 在下一条目边界停止，`OPERATION_CANCELLED`。

### 12.2 可清理目标（与 filesystem DeleteKind 一一对应）

| 条目 | DeleteKind | 目标 |
| --- | --- | --- |
| `uv-cache` | `DeleteUVCache` | `runtime/cache/uv` |
| `download-temp` | `DeleteDownloadTemporary` | `runtime/cache/downloads` |
| `python-cache` | `DeletePythonCache` | `repo/` 下每个 `__pycache__` 目录 |
| `repo-update-*` | `DeleteRepositoryUpdate` | app-root 下 `repo.update-<operationId>` |

> T3.5 修订：按架构设计数据分类表补充 `build-cache` 条目（
> `filesystem.DeleteBuildCache` → `runtime/cache/build`），与实现对齐。

保护名单来自 `config.Layout.ProtectedRootDirs()`（config/data/history/script/
debug/plugins/logs）以及 filesystem 内建拒绝（app-root、repo、state、
logs、卷根、受管根外）。所有递归删除只经 `filesystem.Operator.RemoveTree`，
不直接调用 `os.RemoveAll`；Junction/符号链接/身份变化/逃逸由 filesystem
失败关闭。

### 12.3 repo.update-* 状态与身份匹配

1. 枚举 app-root 下名称匹配 `repo.update-<operationId>` 的目录，且
   `config.Layout.RepoUpdateDir(operationId)` 规范化后与目录一致；
2. 用 `state.Store.ReadTransaction(ctx, TransactionUpdate)` 读取 update 事务：
   - 事务缺失 → 身份不明 → 该条目 failed（失败关闭，不删除）；
   - 事务 `OperationID` 与目录后缀不一致 → 身份不匹配 → failed；
   - 事务存在且匹配：cleanup 已持有 mutation Mutex，任何在途 update 都
     不可能并发持有该 Mutex，因此用 `state.InspectTransaction` +
     PID 探针分类：PID 已退出 → stale，允许删除；PID 仍存活 →
     inconsistent，失败关闭；探针失败 → unknown，失败关闭；
3. 只删除 `Activity=stale` 且身份完全匹配的目录；
4. 删除请求 `OperationID` 使用 update 事务的 operationId（与目录后缀一致），
   满足 `DeleteRepositoryUpdate` 的精确路径授权。

### 12.4 失败与副作用语义

- 每个条目独立执行，互不阻断：先发射 running progress，再执行，再发射
  succeeded / skipped / failed；
- 任一条目 failed → 命令失败，`error` + `result` 使用
  `GIT_REPO_CLEANUP_FAILED`（退出码 40、retryable、remediation
  `cleanup`/`open-log`），details 保留完整条目清单（已删/跳过/失败）与计数；
- 目录不存在视为 skipped（幂等成功）；空根不创建任何目录；
- `result.details` 只含稳定条目 ID、状态、计数和简短原因，不泄漏绝对路径；
- 审计：cli 提供实现 `filesystem.Auditor` 的 stderr 审计记录器（诊断通道），
  每条删除记录 started/finished 两阶段；测试注入内存 auditor 断言审计事实。

## 13. context、错误链与退出码映射

- ctx 从 `Execute` 贯通到服务与所有 filesystem/lock/state 调用；
- 服务错误保留底层链（`fmt.Errorf(...%w...)`），cli 只读取
  code/stage/message/details，不吞链；
- 映射顺序：`context.Canceled` → `OPERATION_CANCELLED`(130)；
  lock 冲突原样保留；cleanup 其余失败 → `GIT_REPO_CLEANUP_FAILED`(40)；
  doctor 服务级失败 → 最接近的冻结码（构造期故障 → `MUTEX_OPERATION_FAILED`
  或 `INVALID_ARGUMENT` 之外，按实际来源）——本阶段 doctor 正常路径不失败；
- 退出码只来自 `LookupErrorDefinition`，绝不手工编造。

## 14. 测试分层与固定命名

- 单元：cli 解析/退出码/help/协议边界（`internal/cli` 同包白盒）；
- 契约：每个真实命令一行 `contracttest.Register(t, "<command>", runner)`，
  runner 经 `cli.Execute` 运行真实命令（版本/doctor 的失败与取消场景使用
  cli Option 注入服务替身；cleanup 使用真实服务 + 夹具自然失败/取消）；
- 服务：doctor/cleanup 包内测试，夹具目录 + 注入探针，测试前后对目录树与
  文件内容做比较证明无副作用；
- 扫描：stdout 所有权扫描、路径归属扫描沿用仓库既有命令；
- 固定测试函数名见各 Task 计划，不得改名。

## 15. 实施顺序与收口

严格 TDD：每个 Task 先落计划中固定的 RED 测试并确认失败原因，再最小实现，
定向测试全绿后执行标准验证门、stdout 扫描、`git diff --check`，独立提交；
随后单独回写 `doc/任务拆分.md` 进度并提交。M3 全部完成后归档设计、
删除已执行计划、更新导航、跑全量验证与 race，再 fast-forward 合入 main。
