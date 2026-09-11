# 全仓库代码 Review 记录

本文件记录对 AUTO-MAS Runtime 的工程级 Review 结论。每个问题先记录、后修复；
未经证实的怀疑记为「需要进一步验证」，不直接改代码。

## Review 元信息

- 基线：`git status` 干净（仅 `doc/current/M9/` 为未跟踪），代码未改动前完成第一轮。
- 范围：`cmd/` 与 `internal/` 全部 296 个 `.go` 文件（165 个非测试 + 131 个测试，
  合计约 10.8 万行，其中非测试代码约 4.15 万行），覆盖 protocol、backend、process、
  filesystem、cleanup、state、lock、gitrepo、uv、mirror、health、doctor、cli、
  telemetry、logging、config、release、version。
- 方法：按域并行深读 + 对每个候选结论回读源码复核，排除「看起来奇怪」但实际在
  别处已处理的误报；结论均附实际行号与引用代码。
- 重点：并发、context 取消、Windows 进程与句柄、文件删除边界、原子写、NDJSON 契约、
  故障恢复、错误码映射、敏感信息外泄。不把代码风格当 Review 内容。

## 验证环境限制（影响哪些结论可被实测）

Review 在 Linux 容器内进行，仓库通过 Windows 挂载访问。**实测覆盖率远低于
「go test ./... 全绿」给人的印象**，这里给出精确口径。

可执行：

- `GOOS=windows` 的 `go build ./...` 与 `go vet ./...`（含全部 `*_windows*.go`）；
- `GOOS=windows go test -c`：可以逐包编译测试二进制，因此**所有测试代码都被类型检查**，
  但二进制无法在本环境运行；
- `gofmt`；
- 跨平台包的 `go test` 与 `go test -race`。

不可执行，按口径拆开：

- **20 个包里有 12 个在 Linux 上根本无法编译**，因此其测试一次也没运行过：
  `cmd/auto-mas-runtime`、`internal/backend`、`cleanup`、`cli`、`doctor`、
  `filesystem`、`gitrepo`、`logging`、`mirror`、`release`、`state`、`uv`。
  根因是 `internal/filesystem` 只有 Windows 实现（`pathAPI`、`pinnedObject`
  没有非 Windows 版本），凡是直接或间接依赖它的包都编译失败。
  这 12 个包共 **94 个测试文件**（`cmd/auto-mas-runtime` 本身没有测试文件）。
  逐包实测结果（`go build <pkg>` 成功与否）而非 `go list` 推断：`go list ./...`
  在 Linux 上对全部 20 个包都返回成功，不能用来推导可运行集合。
- 剩下 8 个可编译包共 **37 个测试文件**，其中 9 个带 `//go:build windows`
  被跳过（`internal/lock` 全部 8 个都跳过，所以该包在 Linux 上显示
  `[no test files]`；`internal/process` 跳过 1 个）。
- 净结果：`internal/` 下 **131 个测试文件里只有 28 个真正执行过**（约 21%）。
- 执行过的那 28 个里，`internal/config` 有 4 个叶子子测试在 Linux 上确定性失败
  （`TestNewLayout_ResolvesExplicitBase/mixed_separators_with_parent_and_trailing_separator`
  与 `TestNewLayout_RejectsInvalidInputs` 的 `volume_relative_app_root`、
  `volume_root`、`unc_share_root`，全是纯 Windows 路径语义），
  这是**与本轮改动无关的既有环境基线**——`internal/config` 未被本轮任何改动触及
  （`git diff --name-only` 中没有该包的任何文件）。
- `golangci-lint`：环境未安装。

因此：**单元测试通过不等于并发与 Windows 语义正确**。本轮修复分布在
`internal/uv`（H-1、M-1、M-3）、`internal/cli`（H-6、M-6）、
`internal/mirror`（M-2、L-2）、`internal/backend`（M-5）、`internal/lock`（M-4、R-1）
和 `README.md`（L-1）。前四个包在 Linux 上不可编译，`internal/lock` 的测试全部
带 Windows 构建标记——**所以本轮新增的回归测试没有任何一条在本环境实际运行过，
全部只做到了 `GOOS=windows go test -c` 的类型检查**。这些测试是否真的能捕获
对应缺陷，必须在 Windows 上按文末清单跑一遍才算验证完成。

这个缺口不是理论风险：**R-1 就是它直接造成的漏网**——M-4 的修复越范围改了
错误语义，与 `internal/lock` 两个既有测试的断言冲突，而该包在本环境
显示 `[no test files]`，类型检查看不出断言层面的矛盾，只有在 Windows 上
真正运行才会暴露。第二轮逐文件复查 `git diff` 时才发现并回退。

## 问题统计

| 严重级别 | 数量 | 已修复 | 暂不修改 | 需要进一步验证 |
| --- | ---: | ---: | ---: | ---: |
| Critical | 0 | 0 | 0 | 0 |
| High | 7 | 3 | 4 | 0 |
| Medium | 9 | 6 | 3 | 0 |
| Low | 3 | 2 | 1 | 0 |
| 需要进一步验证 | 2 | — | — | 2 |

合计 21 项，其中 11 项已修复。

第一轮 19 项（H-1…H-6、M-1…M-8、L-1…L-3、V-1、V-2），
第二轮复查本轮改动又发现 2 项（R-1、R-2），按级别计入上表：
R-1 记为 High、R-2 记为 Medium。

没有 Critical：所有会导致用户数据丢失或协议不可用的路径都已有防线，
本轮发现的是「防线之外的异常路径」。

「暂不修改」的 8 项分成两类，理由都写在各条目里：
一类需要 Windows 运行期验证而本环境不具备（H-2/H-4/H-5/M-8/R-2），
一类需要先改冻结契约或做设计级改动，超出「最小范围修复」（H-3/M-7/L-3）。

第一轮的候选项里有若干条在回读源码后**被证伪并已剔除**，不计入上表，例如：
`isLinkDirEntry`（cleanup.go:394-400）对 Junction 的 `ModeIrregular` 判定完整且有
注释说明成因；`mirror.DefaultCatalog()` 是纯内存构造、不产生网络副作用，
因此不存在「离线模式下先联网」的问题；`defaultShutdownTimeout` 实为 5s
而非我最初记录的 60s。此外，上一轮 Review 遗留的 Important-1
（uv 子进程继承遥测环境变量）经复核**已修复**：`runner.go:517-523` 的
`isRuntimeOnlyEnvironmentKey` 用 `strings.EqualFold` 覆盖全部四个键，
且调用方无法经 `RunOptions.Environment` 重新注入（:459-462）。

## High

### [High] H-1 uv 执行器在读完管道前调用 Wait，成功的 uv 运行被判为执行失败

**位置**：`internal/uv/runner.go:265-296` `UVRunner.Run()`

**当前行为**：管道来自 `command.StdoutPipe()` / `command.StderrPipe()`（:154、:164），
两个读取 goroutine 启动后立即 `Wait`，读取结果在 `Wait` 之后才收取：

```go
go readUVStream("stdout", stdout, lineSink, outputOverflow, stdoutResult)
go readUVStream("stderr", stderr, lineSink, outputOverflow, stderrResult)
waitErr := command.Wait()
...
stdoutValue := <-stdoutResult
stderrValue := <-stderrResult
```

**问题**：`os/exec` 明确规定 `StdoutPipe` 返回的管道会被 `Wait` 关闭，因此
「在从管道读完之前调用 `Wait` 是错误的」。子进程退出后 `Wait` 关闭父端 fd，
仍在 `ReadSlice` 中或尚未被调度的读取会返回 `os.ErrClosed`；该错误经
`recordFirstError`（:647）写入 `streamErr`，最终在 :288 被转成
`CodeUVExecFailed`「uv 输出读取失败」——即使 uv 自身退出码为 0。

**触发条件**：写完即退出的短命 uv 调用，读取 goroutine 尚未被调度完成。
即 `uv --version`（`CheckVersion`）、`uv python find`、`uv lock --check`。

**影响**：bootstrap / 环境校验随机失败，且不可重现地报错。更糟的是 stdout 丢失会让
`uvVersionOutputMatches`（:341）看到空输出并抛出 `CodeUVVersionMismatch`，
驱动 `ensure` 删除并重新下载一个本来完好的固定版本 uv（`bootstrap.go:294-303`）。

**严重程度**：High。它把「正常成功」变成「随机失败 + 不必要的重新下载」，
是长期使用中最容易被归因为「网络不稳定」的一类偶发故障。

**修复方案**：改用自持的 `os.Pipe()`，把读端所有权从 `exec` 手里拿回来：
`command.Stdout` / `command.Stderr` 指向写端，`Start` 之后立刻关闭父进程持有的
写端副本，读端由本函数 `defer` 关闭。这样 `Wait` 无从关闭读端，
EOF 只取决于子进程树是否还持有写端。

**为什么不是「把 channel 收取移到 Wait 之前」**：那是 `os/exec` 文档给出的写法，
但会**去掉一个现有的反挂死性质**——今天若有孙进程继承了写端并卡住不退出，
`Wait` 关闭读端会强制读取结束；先收取就会一直阻塞到 ctx 取消。
自持管道两个性质都保留：正常路径读到真 EOF，卡死路径由既有的
`job.Close()`（:271，关闭 Job 触发 `KILL_ON_JOB_CLOSE` 杀掉整棵树）
和 ctx 取消时的 `job.Terminate(1)`（:232）关掉全部写端句柄，仍然产生 EOF。
现有的「Wait → 关 Job → 收 channel」顺序因此完全不变。

**修复状态**：已修复。

### [High] H-2 后端日志泵与健康检查 goroutine 无 recover，panic 直接终止进程且不发 result

**位置**：`internal/process/managed_windows.go:331-344`；
`internal/backend/supervisor.go:565-591` `streamSink()`；
`internal/backend/control.go:958,1505,1609,1805`；
`internal/health/checker.go:313,470,591`

**当前行为**：日志泵 goroutine 是裸启动的：

```go
go func() {
    defer readers.Done()
    drainStream(sinkContext, StreamStdout, stdout, sink, managed.recordSinkError)
```

`sink` 是 `streamSink`（supervisor.go:565），链路为
`gate.Emit` → `emitter.EmitLog` → 协议渲染器。
`grep -rn "recover()" internal/process internal/backend internal/health`
在非测试代码中**零命中**。

**问题**：会话层只为两个 goroutine 装了 recover（`internal/cli/telemetry.go:213-258`
的 `invokeSessionRun`、`runControlReaderSafely`）以及主 goroutine
（`session.go:80-91`）。Go 的语义是「panic 的 goroutine 会终止整个程序」，
其他 frame 的 defer recover 不会执行。因此日志链路上任何 panic 都不可恢复。

**触发条件**：`drainStream`、`streamGate.Emit`、`EmitLog` 或渲染器中的任何 panic，
例如 `request.Emitter` 为 nil，或渲染器在对抗性日志字节上的缺陷。

**影响**：不发出任何 `result`，Electron 永久等待终态事件；进程以 Go panic 的退出码
**2** 退出，与文档中「2 = 参数错误」冲突；裸 Go 栈打到 stderr。
仓库已在会话层为同类风险装了 recover，因此这是不一致而非有意设计。

**严重程度**：High（后果严重，但目前没有已证实的活跃 panic 路径）。

**修复方案**：在每个长期存活的 goroutine 入口加 `defer` recover，将 panic 转换为
既有的 sink 错误通道（`recordSinkError`）或 gate fault，使其走既有的
`CodeInternalError` 失败路径并保证 `result` 仍被发出。

**修复状态**：暂不修改。理由：主要落点 `managed_windows.go` 与
`checker.go` 的 goroutine 在本环境**无法运行验证**（Windows-only 与需要真实后端），
而 recover 的引入会改变失败传播路径——若转换后的错误未被正确归并，可能把
「进程崩溃」换成「静默挂死」，那是更坏的结果。建议作为独立任务在 Windows 上按
「先写 panic 注入测试（红）→ 加 recover（绿）」的顺序实施。

### [High] H-3 result 渲染失败后没有终态兜底，操作可能完全不发终态事件

**位置**：`internal/protocol/emitter.go:255-273` `emitPreparedLocked()`；
调用方 `internal/cli/session.go:220-231` `emitSuccess()`、`:233-262` `emitFailure()`

**当前行为**：渲染失败时回滚终态占位：

```go
if err := renderEvent(e.output.renderer, event); err != nil {
    if terminalReserved {
        e.output.terminal = false
    }
```

注释说这是「保留非粘性编码错误的重试语义」，但两个调用方都不重试：

```go
if err := emitter.EmitResult(result); err != nil {
    writeDiagnostic(deps.io, err)
    return protocol.ExitCodePreconditionFailed, false
}
```

**问题**：回滚保留了一个没人使用的重试能力，于是每次 `result` 编码/渲染失败都以
「线上零个 `result`」收场。`emitFailure` 更糟：`EmitError`（:240）可能成功而
`EmitResult`（:257）失败，留下一个有 `error` 无 `result` 的流，违反
「`result` 恰好一次且最后」这一协议不变量，原因只记录在 stderr。

**触发条件**：`result.details` 中出现不可 marshal 的值（NaN/Inf 浮点、channel、
func、循环 map），或 stdout 出现非 EPIPE 的瞬时写错误。

**影响**：Electron 收到 `hello`（可能再加 `error`）之后就是静默，永久等待终态；
进程却以 20 或业务退出码退出。

**严重程度**：High（后果是 Electron 永久挂起）。可达性目前较低：已排查全部
details 构造点，未发现不可 marshal 的值真正流入（`internal/mirror/downloader.go:631`
的 `Percent` 是唯一浮点，且不进入 `protocol.ProgressEvent.Percent`）。

**修复方案**：在 `emitSuccess` / `emitFailure` 中为 `EmitResult` 失败增加一次
兜底重试——用清空 `details` 的最小 `result` 再发一次；仅当兜底也失败才放弃。

**修复状态**：暂不修改。理由：这触及**冻结的协议终态语义**，按 AGENTS.md 6.5，
应先修 `doc/架构设计.md`「最终结果契约」明确「details 不可编码时降级为空 details」
再改代码。本轮不擅自改契约。已在文末「建议后续任务」登记。

### [High] H-4 无法分类的 repo.previous-* 会在 swap 提交点之后永久卡死恢复

**位置**：`internal/gitrepo/recovery.go:188-191`、`:291-300` `classifyPath()`、
`:385-393` `recoverSwap()`

**当前行为**：`previous` 的分类不允许「不完整」：

```go
previous, err := r.classifyPath(ctx, paths.previous, false)
if err != nil {
    return RecoveryResult{}, r.classificationError(ctx, transaction.state.Stage, "previous_unknown", err)
}
```

`allowIncomplete=false` 使 `reader.Inspect` 失败或 git 身份不符合都返回错误
（:294 `return recoveryPath{}, fmt.Errorf("inspect recovery repository: %w", err)`），
最终变成 `UPDATE_STATE_AMBIGUOUS` + `"previous_unknown"`。

**问题**：`repo.previous-*` 是一个**即将被删除或用于回滚**的目录，只需要它的
「目录身份」，而目录身份在 :266-281 已经通过 `PinManagedDirectory` 取到了；
它的 **git 有效性无关紧要**。代码把「git 结构合规」与「可识别」混为一谈。
`recoverSwap` 中 `previous.kind == recoveryPathIncomplete` 的判断（:386-388）
因此成为不可达死代码——这反过来说明作者原本预期 `Incomplete` 在这里是可达的。

**触发条件**：对一个**本来就不健康**的仓库执行 sync（这恰恰是最常见的 sync 动因）。
`Service.Sync` 不以健康为前提（service.go:392-396 只在健康且版本匹配时短路），
swap 成功、`CommitEnvironment` 写入 `repository_changed` 之后，在退休目录清理
（swap.go:244-254，删除大 `.git` 很慢）期间被硬杀。
此时 `repo`=已校验的目标、`update` 缺失、`previous`=旧的不合规仓库。

**影响**：之后每次 `workspace sync` 都从 `runtime.Recover`（service.go:349）开始并
死在同一处分类。`repo` 其实已经是被证实的目标版本、只剩垃圾待删，更新事务却
永远无法摘除。`internal/`、`cmd/` 全域检索确认没有任何其他代码删除
`repo.previous-*`，因此**产品内无自救路径**，只能人工删目录。

**严重程度**：High。

**修复方案**：对 `previous` 传 `allowIncomplete=true`，并在 `recoverSwap` 中把
`previous.kind == recoveryPathIncomplete` 与 `recoveryPathValid` 同等对待
（它只会被删除或整体回滚，不需要 git 语义）。

**修复状态**：暂不修改。理由：这是**中断恢复分类逻辑**，一处判断放宽就可能让
恢复在本该 fail-closed 的形状上做出错误动作，而其后果正是本条要避免的
「不可恢复 / 数据丢失」。相关代码路径依赖 Windows 句柄级 pin
（`filesystem.PinManagedDirectory`），在本环境**无法运行任何验证**。
建议作为独立任务在 Windows 上以 TDD 实施（先构造「previous 为非 git 目录 +
stage=cleanup」的红灯用例）。

### [High] H-5 swap 阶段 repo 损坏时不可恢复，因为 allowDamagedRepository 不含 swap/cleanup

**位置**：`internal/gitrepo/recovery.go:177-183`；`internal/gitrepo/swap.go:129-133`

**当前行为**：

```go
allowDamagedRepository := transaction.state.Stage == protocol.StageWorkspaceClone ||
    transaction.state.Stage == protocol.StageWorkspaceVerify
repository, err := r.classifyPath(ctx, paths.repository, allowDamagedRepository)
```

在 `stage=swap` / `cleanup` 下，损坏的 `repo` 直接产生错误 →
`UPDATE_STATE_AMBIGUOUS` + `"repository_unknown"`，`recoverSwap` 的 switch 根本进不去。

**问题**：`Swapper.Swap` 在 swap.go:131 于**任何 rename 副作用之前**就把
`stage=swap` 落盘。因此该窗口内崩溃虽被记为 swap 阶段，磁盘形状仍是完全的
swap 前状态且**完全确定**（`repo`=旧、`update`=已校验目标、`previous` 缺失）。
安全动作是明确的（继续 swap 或回滚到干净起点），分类却仅因旧仓库解析不出 git
结构而拒绝处理。

**触发条件**：`Service.Sync` 不以健康为门槛，不健康的 `repo` 也会走到 Swap
（`check.directoryIdentity` 在无效路径上仍被填充，service.go:247，
故 swap.go:184 不会拦下），在 swap.go:131 之后、第一次 rename 之前被杀。

**影响**：与 H-4 同构的永久卡死，只是发生在提交点之前：后续每次 sync 都返回
`UPDATE_STATE_AMBIGUOUS`，更新事务永不清除，产品内无自救路径。

**严重程度**：High。

**修复方案**：把 `allowDamagedRepository` 扩展到 `StageWorkspaceSwap`
（`cleanup` 阶段的 `repo` 已是目标版本，不适用），并在 `recoverSwap` 中让
`repository.kind == recoveryPathIncomplete` 走「回滚到 swap 前」分支。

**修复状态**：已由 T4.9 `9fe260a` 修复并在 Windows 上完成定向重复、Git Recovery/Replacement
组件矩阵、标准验证门和全仓 race。Recovery 只在 `workspace.swap` 阶段把可安全 pin、但 Git
身份无效的旧 repo 视为 incomplete，并仅在目标 update 已验证且 previous 缺失时回滚到 swap
前状态；其他不确定形状仍失败关闭。

### [High] H-6 version 命令把版本源失败伪装成协议输出失败

**位置**：`internal/cli/version.go:40-56` `runVersion()`；
版本源实现 `internal/version/version.go:29-32`

**当前行为**：

```go
info, err := source(ctx)
if err != nil {
    if errors.Is(err, context.Canceled) { return sessionSuccess{}, err }
    var operationErr operationError
    if errors.As(err, &operationErr) { return sessionSuccess{}, err }
    return sessionSuccess{}, &commandError{
        code:  protocol.CodeOutputWriteFailed,
        stage: protocol.StageRuntimeHandshake,
```

**问题**：`doc/架构设计.md` 明确要求「任何命令都不得把内部故障伪装成输出写失败」，
`session.go:270-273` 也重申兜底码不得是 `OUTPUT_WRITE_FAILED`。此处根本没有向协议
通道写入任何内容——失败的是版本信息查询。且 `context.DeadlineExceeded` 不被上面的
`context.Canceled` 分支捕获，而 `classifyFailure` 在 `session.go:275` 先检查
`findOperationErrorCode(err, CodeOutputWriteFailed)`、晚于 `:299` 的取消分支，
于是这个显式的 `OUTPUT_WRITE_FAILED` 会胜出。

**触发条件**：`versionSource` 返回任何非 `context.Canceled` 的错误。当前生产路径
（`context.Background()`，无 deadline）不可达，但 `versionSource` 是注入的依赖
（`deps.options.versionSource`），契约上必须正确。

**影响**：退出码 20 + `OUTPUT_WRITE_FAILED`，Electron 会理解为「协议通道已损坏」
并对一个普通内部错误提示 `open-log` / `contact-support`；同时因为只有
`INTERNAL_ERROR` 会上报，遥测也保持静默。

**严重程度**：High（契约层面的错误码误用，直接违反架构文档的显式禁令）。

**修复方案**：兜底码改为 `protocol.CodeInternalError`，并把
`context.DeadlineExceeded` 与 `context.Canceled` 一并交给会话层的取消分支处理。

**修复状态**：已修复。

## Medium

### [Medium] M-1 uv 诊断输出达到 4 MB 上限被当作执行失败

**位置**：`internal/uv/runner.go:581-591` `readUVStream()`

**当前行为**：

```go
if builder.Len() < maxUVOutputBytes {
    remaining := maxUVOutputBytes - builder.Len()
    if len(value) > remaining {
        _, _ = builder.Write(value[:remaining])
        recordFirstError(&streamErr, errors.New("uv output exceeds capture limit"))
    } else {
        _, _ = builder.Write(value)
    }
} else {
    recordFirstError(&streamErr, errors.New("uv output exceeds capture limit"))
}
```

**问题**：`maxUVOutputBytes`（4 MB）是**诊断快照**的容量上限，不是流上限；
流上限是另一个 16 MB 的 `outputOverflow`，后者会主动杀掉 uv（:270-283），语义正确。
4 MB 这一层只影响「留多少输出用于报错」，超出应当截断并继续，但这里写进了
`streamErr`，于是 :288 把整次运行判为 `CodeUVExecFailed`。

**触发条件**：uv 输出超过 4 MB。首次冷启动 `uv sync` 在依赖多、走镜像、
或开了 `-v` 的情况下完全可能达到；输出量取决于依赖数量而非随机因素。

**影响**：`dependencies sync` / `rebuild` / `bootstrap` 在依赖树足够大时**确定性失败**，
且重试无用（同样的依赖树产生同样多的输出）。用户看到的是「uv 执行失败」，
真实原因是「日志太长」。这是本轮里最接近「用户一定会遇到」的一条。

**严重程度**：Medium（确定性失败，但只在大输出场景出现，且不损坏状态）。

**修复方案**：截断到上限即止，不写 `streamErr`；在快照末尾追加一行截断标记，
使报错里能看出输出被截断过。16 MB 流上限的 kill 行为完全不动。

**修复状态**：已修复。

### [Medium] M-2 下载器的进度探测失败分支泄漏 HTTP 响应体与 context

**位置**：`internal/mirror/downloader.go:470-480` `Downloader.Download()`

**当前行为**：同一函数内其余四个失败分支（:441、:450、:460、:472 附近）都做
`failure.Err = errors.Join(failure.Err, closeResponseAndCancel(handle))`，
只有「未知大小时补建 reporter」这一支没有：

```go
if reporter == nil {
    reporter = newProgressReporter(...)
    if err := reporter.report(0, true); err != nil {
        return DownloadResult{}, d.abortFailure(
            ctx, session, newDownloadFailure(FailureProgress, 0, err),
        )
    }
}
```

**问题**：`abortFailure` 只回滚文件系统 session，不碰 `handle`。于是
`response.Body` 不会被 `Close`、`handle.cancel` 不会被调用：连接不归还连接池，
派生 context 的资源直到父 context 结束才释放。

**触发条件**：响应缺少可用的 `Content-Length`（分块传输、部分 CDN、代理）
**且** 首个 `progress` 事件写 stdout 失败——即 Electron 已停止读取而管道尚未破裂。

**影响**：单次调用泄漏一个连接和一个 context。`Download` 在镜像轮换中被反复调用
（`bootstrap.go` 的 rotation、Python 安装），因此一次镜像轮换可以累积多次泄漏。
不会立即可见，属于典型的「长期使用后才暴露」。

**严重程度**：Medium。

**修复方案**：与其余分支保持一致，在返回前 join `closeResponseAndCancel(handle)`。
这是把已有的清理模式补齐，不引入新机制。

**修复状态**：已修复。

### [Medium] M-3 bootstrap 的镜像轮换忽略 DownloadFailure.Published，已落盘的成品被重试掉

**位置**：`internal/uv/bootstrap.go:540-556` 下载轮换回调；
`internal/mirror/downloader.go:85-91` `DownloadFailure`、`:515-527` 发布语义

**当前行为**：回调只看 `downloadErr` 是否为 nil：

```go
if downloadErr == nil {
    downloadedPath = downloadResult.Path
    return mirror.AttemptOutcome{Kind: mirror.OutcomeSucceeded}
}
```

而 `Download` 有一个刻意设计的中间态：校验通过、成品已 rename 到最终位置之后
若仍产生错误，返回的 `DownloadFailure.Published` 为 `true`，表示
**产物是有效的，只是收尾步骤失败**。

**问题**：`Published=true` 的失败被当成普通失败，触发下一个镜像重试；
下一次 `Download` 发现目标已存在，返回 `ErrDestinationExists`，同样算失败；
轮换耗尽后以 `MIRROR_EXHAUSTED` 结束——**而磁盘上躺着一个字节精确、
校验通过的归档**。

**触发条件**：校验通过、rename 完成之后的收尾失败（临时目录清理失败、
末次 progress 写失败等）。

**影响**：`environment ensure` / `bootstrap` 以网络类错误失败，用户被引导去查网络，
实际上产物已经就绪；重跑同样失败（`ErrDestinationExists` 是稳定的），
需要人工删除临时归档才能恢复。

**严重程度**：Medium。

**修复方案**：仓库里已有现成范式——同文件 :739-742 的
`isDownloadIntegrityFailure` 就是用 `errors.As` 取 `*mirror.DownloadFailure`。
照此新增判定：`Published` 为真时，把该次结果视为成功（记录 `downloadedPath`），
并把收尾错误降级为 warning，而不是让轮换继续。

**修复状态**：已修复。

### [Medium] M-4 命名 Mutex 释放线程的等待循环无上界，可能永不返回并耗尽内存

**位置**：`internal/lock/worker_windows.go:283-303` `finishThread()`

**当前行为**：

```go
for {
    result, err := s.api.waitForSingleObject(s.thread, threadWait)
    if err != nil {
        waitErr = errors.Join(waitErr, &OperationError{...})
        runtime.Gosched()
        continue
    }
    if result != waitResultObject0 {
        waitErr = errors.Join(waitErr, &OperationError{...})
        runtime.Gosched()
        continue
    }
    break
}
```

`threadWait` 为 `INFINITE`。

**问题**：没有次数上限、没有 ctx 检查、不区分「瞬时错误」与「永久错误」。
`WAIT_FAILED`（句柄无效、已关闭、非 wait 对象）是永久错误，重试永远得到同样结果。
`runtime.Gosched()` 只让出调度，不阻塞，所以这是一个纯忙等。
同时每轮都往 `waitErr` 里 join 一个新的 `*OperationError`，错误链无界增长。

**触发条件**：`s.thread` 句柄失效——重复 `Close`、句柄被外部关闭、
`CreateThread` 返回了伪句柄，或 API 层被注入的实现返回持久错误。

**影响**：`Close()` 永不返回 → 持有锁的操作无法收尾 → 一个 CPU 核心 100%
→ 错误链持续分配直到 OOM，且命名 Mutex 永不释放，后续所有需要该锁的操作
都撞 `LOCK_HELD`（退出码 70）。这是「单点故障放大成整机不可用」的形状。

**严重程度**：Medium（后果重，但触发前提是句柄本身已经异常）。

**修复方案**：给循环加一个小的常量上界，只保留首个错误（沿用文件内既有的
「首错优先」风格），到达上界后带着错误跳出并继续关闭句柄——保证
`Close()` 一定返回，且不吞掉诊断信息。

**修复状态**：已修复。行为改动仅限于「原本会永不返回的路径」，
正常路径（首次 wait 即成功）完全不变。

### [Medium] M-5 finishControlCancel 的完成循环没有超时与 ctx 分支，可无限期挂起

**位置**：`internal/backend/control.go:1800-1873` `finishControlCancel()`

**当前行为**：循环只有两个 case——`attempt.results` 与 `cleanupDone`：

```go
for !controlEnded || !cleanupFinished {
    select {
    case outcome := <-attempt.results:
        ...
    case <-cleanupDone:
        cleanupFinished = true
    }
}
```

同文件的兄弟函数 `closeControlAndDrain` 与 `drainUntilTerminal` 都用
`controlDrainTimeout` 给自己设了上界。

**问题**：`cleanupDone` 是 buffered(1) 且只触发一次，所以 `cleanupFinished` 能到位；
但生产路径上 `BeforeControlClose` 始终非 nil，`controlEnded` 初值为 `false`，
唯一的出路是转发 goroutine 往 `attempt.results` 投递一次结果。
若该 goroutine 已经因为管道错误永久退出，就没有任何投递会到来，
循环没有 timer 也没有 `ctx.Done()`，于是永久阻塞。

**触发条件**：取消收尾期间控制通道转发 goroutine 已提前终止
（命名管道断开、后端已死）。

**影响**：`cancel` 之后 Runtime 不退出、不发 `result`。用户按了取消，
进程挂在那里，Electron 等终态事件等不到——这是取消路径上最不该发生的形状。

**严重程度**：Medium。

**修复方案**：与兄弟函数保持一致，加一个 `controlDrainTimeout` 分支
（超时即认为控制通道已终结并记录原因）。不改变正常路径的时序。

**修复状态**：已修复。

### [Medium] M-6 package-index 镜像的拒绝点在环境已被改动之后

**位置**：`internal/uv/dependencies.go:234-242` `validateRequest()`；
`internal/cli/flags.go:100-144` 全局镜像解析

**当前行为**：`package-index` 是唯一不被依赖同步接受的镜像类型，
但拒绝发生在 `validateRequest` 内部，而该函数在 `ensureM5UV`、
`workspace.Sync`、`PreparePython` 之后才被调用。全局解析层（flags.go）
接受 `package-index` 且不做任何命令级检查。

**问题**：一个纯粹的**参数错误**要等到「uv 已下载、仓库已同步、Python 已安装」
之后才报出来。按 AGENTS.md 的失败语义，参数校验应当在产生副作用之前完成。

**触发条件**：`bootstrap` 或 `repair` 带 `--mirror package-index=<键>`。
考虑到 `--mirror` 的四个合法类型里恰好有 `package-index`（README 全局选项表也这么写），
用户按文档拼参数就会撞上。

**影响**：一次失败的 `bootstrap` 已经完成了大量磁盘写入与网络下载
（数百 MB 级），最后以参数错误收场。状态不损坏，但代价与错误性质完全不匹配。

**严重程度**：Medium。

**修复方案**：不在全局解析层拒绝——那会连带影响本来允许该参数的其他命令
（例如 `version --mirror package-index=x` 目前是合法的，改动会扩大破坏面）。
正确的最小修复是在 bootstrap / repair 的**入口处、任何副作用之前**做同样的检查，
复用 `dependencies.go` 已有的错误构造，保证错误码与消息完全一致。

**契约依据**：这不是我的偏好，是已冻结的契约。架构设计.md:449-452 明确要求
「对 `dependencies check/sync/rebuild`、`bootstrap` 和顶层 `repair`，显式
`--mirror package-index=<key>` 必须返回 `INVALID_ARGUMENT`」；任务拆分.md:624
进一步要求「显式首选在 uv 调用前失败关闭」。修复前的代码满足前半句、不满足后半句。

**修复状态**：已修复。新增 `rejectPackageIndexOverride()`（`internal/cli/errors.go`），
在 `runBootstrap()` 的版本解析之后、开状态库之前调用，在 `runRepair()` 的函数首行调用。
错误码、消息与 details 与 `dependencies.go` 完全一致，因此谁先命中对调用方是同一个错误。
全局解析层未改动，`version --mirror package-index=x` 仍然合法。

**连带的测试修正**：`internal/cli/m5_test.go` 的
`TestBootstrapCommand_OrderAndStates` 原本给 `bootstrap` 传
`--mirror package-index=pypi` 并断言**成功**，还断言该首选透传到了依赖请求。
它与上述契约直接冲突，只因依赖服务被换成假实现才没撞上真实拒绝——
即它固化的是「契约未被执行」这一缺陷状态。按「不要为了通过测试修改错误行为」，
判定测试为错误的一方：改用 `--mirror git=cnb` 验证同一条镜像策略透传路径
（断言强度不变），拒绝行为由新增的
`TestM5CommandsRejectPackageIndexOverrideBeforeSideEffects` 覆盖，
该用例同时断言 bootstrap 与 repair 在拒绝前**一次副作用都没有发生**
（无 mutation 锁、无状态库写入、uv/Python/依赖调用计数全为 0）。
`internal/cli/cli_test.go` 的 `TestExecute_MirrorMultipleKindsAccepted`
（`version` 带四种 kind）未受影响，仍然通过。

### [Medium] M-7 streamGate.Emit 持锁写 stdout，父进程停止读取会连带堵住监督循环

**位置**：`internal/backend/supervisor.go:516-536` `Emit()`、`:487-491` `Fault()`

**当前行为**：`Emit` 在持有 `g.mu` 的整个期间调用 `emitter.EmitLog(event)`，
也就是**持锁执行一次 stdout 写**：

```go
func (g *streamGate) Emit(emitter EventEmitter, event protocol.LogEvent) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	...
	if err := emitter.EmitLog(event); err != nil {
```

`Fault()` 同样要拿 `g.mu`，而监督主循环每轮都调用它。

**问题**：当 Electron 不再读取 stdout 且管道缓冲写满时，`EmitLog` 阻塞在系统调用上，
`g.mu` 被一直持有。监督主循环随后在 `Fault()` 上阻塞，**在进入自己的 `select` 之前**
就停住了，于是 stdin 上的 `shutdown` 不再被读取。

**触发条件**：父进程停止消费 stdout（UI 卡死、渲染进程忙、调用方只在结束时读一次）
但管道尚未破裂。这在后端高频输出日志时最容易发生。

**影响**：Runtime 既不推进也不响应 `shutdown`，只能靠杀进程结束——而杀进程正是
本项目反复强调要避免的收尾方式（Job Object 能兜住 Python 树，但这条路径上
Runtime 自身的优雅退出被绕过了）。

**严重程度**：Medium。

**修复方案（不采用）**：把 `EmitLog` 移出锁。

**修复状态**：暂不修改。理由：在锁外写会**破坏事件顺序**——两个并发 `Emit`
（stdout 泵与 stderr 泵）可能交错写入同一行缓冲，这比阻塞更糟，且直接违反
「stdout 每行恰好一个 JSON 对象」。真正的根因是「协议输出是无界阻塞写」，
正确解法是给 stdout 写引入独立的单写者 goroutine + 有界队列（队列满即
`OUTPUT_WRITE_FAILED`），那是设计级改动，超出「最小范围修复」的边界。
已登记为后续任务，本轮只记录。

### [Medium] M-8 CREATE_SUSPENDED 到 ResumeThread 之间，Runtime 被硬杀会留下永久挂起的孤儿进程

**位置**：`internal/process/managed_windows.go:98` 创建、`:166-169` 分配 Job、
`:181-191` 恢复线程；清理路径 `:104-150` `cleanupStarted()`

**当前行为**：后端以 `CREATE_SUSPENDED | CREATE_NO_WINDOW` 启动，随后
`assign(job, processValue)` → `initialThread` → `resumeThread`。函数内部所有失败分支
都走 `cleanupStarted`，它逐级尝试 terminate、`terminateSuspendedInitialThread`、
`job.Close()`，并最终确认终止——这部分是完善的。

**问题**：`cleanupStarted` 只覆盖**本函数返回错误**的情形。如果 Runtime 自身在
:98 与 :169 之间被硬杀（任务管理器、`Stop-Process`、OOM killer），子进程已经存在
但**尚未加入 Job Object**，于是 `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` 不适用；
又因为它处于 suspended，永远不会执行、不会退出、不占 CPU。

**触发条件**：在这个窗口内硬杀 Runtime。窗口很窄（三次系统调用），
但 `backend supervise` 是最常被用户直接结束的命令。

**影响**：留下一个不可见的挂起进程，占用 PID 与已提交内存，且不会被任何
后续 Runtime 调用回收（按项目红线，**禁止按进程名杀进程**，所以不能靠名字清理）。
下一次 `backend supervise` 因固定端口 36163 未被占用（挂起进程没 bind）可以正常启动，
所以用户不会察觉——属于典型的长期残留。

**严重程度**：Medium。

**修复方案（不采用）**：Windows 上无法完全消除这个窗口——「先创建再分配」是
`CREATE_SUSPENDED` 方案的固有代价。理论上可用
`PROC_THREAD_ATTRIBUTE_JOB_LIST` 在创建时直接入 Job，从而把窗口压到零。

**修复状态**：暂不修改。理由：改用 `PROC_THREAD_ATTRIBUTE_JOB_LIST` 需要重写
进程创建路径（`STARTUPINFOEX` + 属性列表），是 Windows-only 代码且本环境
**无法运行任何验证**，风险远大于收益。当前实现已经用 `CREATE_SUSPENDED`
把窗口缩到最小并对所有**可返回**的失败做了完整回收，这是合理的工程取舍。
已在文末登记，供将来在 Windows 上评估。

## Low

### [Low] L-1 README 的禁用遥测示例用了单反引号，渲染成行内代码而非代码块

**位置**：`README.md` 「错误观测」一节的 `AUTO_MAS_TELEMETRY` 示例
（修复时位于 `:167-169`，本轮精简 README 后为 `:159-161`）

**当前行为**：

```text
`powershell
$env:AUTO_MAS_TELEMETRY = 'disabled'
`
```

**问题**：围栏代码块需要三个反引号。单反引号在 GitHub 上渲染为一段畸形的行内代码，
`powershell` 这个语言标记会被当作正文显示出来。

**触发条件**：任何人阅读 README 的「错误观测」一节。

**影响**：文档缺陷，无功能影响。但这是用户复制粘贴的命令，畸形渲染会让人
误以为要连 `powershell` 一起输入。

**严重程度**：Low。

**修复方案**：改为三反引号围栏。

**修复状态**：已修复。同时全文扫描确认 README 没有其他同类畸形围栏。

### [Low] L-2 pumpBody 在读到 (0, nil) 时纯忙等，且该分支不检查 context

**位置**：`internal/mirror/downloader.go:646-672` `pumpBody()`

**当前行为**：

```go
for {
    n, err := body.Read(buffer)
    if n == 0 && err == nil {
        continue
    }
```

**问题**：按 `io.Reader` 契约，调用方「应当把 (0, nil) 视为什么都没发生」，
所以 `continue` 本身是正确处理。问题在于**这条分支不检查 `ctx.Done()`**：
函数里唯一的 ctx 检查在下面的 `select` 发送处，而 (0, nil) 走不到那里。
于是一个持续返回 (0, nil) 的 reader 会让这个 goroutine 无限忙转，
且**无法被取消**——`requestCtx` 取消也不管用。

**触发条件**：`response.Body` 持续返回 (0, nil)。标准库的 HTTP body 不会这样，
但 `body` 是接口，测试替身、未来的解压/限速包装层都可能这样表现。

**影响**：一个 goroutine 占满一个核心且不响应取消；下载超时机制（`readTimer`）
在 `readResponse` 侧仍会触发，所以下载本身会失败退出，但 `pumpBody` goroutine
泄漏且持续烧 CPU 直到进程结束。

**严重程度**：Low（当前生产依赖 net/http，不会触发）。

**修复方案**：在该分支内检查一次 `ctx.Err()`，非 nil 即返回。三行改动，
不改变任何正常路径行为。

**修复状态**：已修复。

### [Low] L-3 --protocol 接受 "01"、"+1" 等非规范写法

**位置**：`internal/cli/flags.go:62-72` `validateOutputAndProtocol()`

**当前行为**：

```go
protocolVersion, err := strconv.Atoi(rawProtocol)
if err != nil {
    return "", fmt.Errorf("invalid protocol %q, want integer", rawProtocol)
}
if protocolVersion != protocol.Version {
    return "", errProtocolMismatch
}
```

**问题**：`strconv.Atoi` 接受 `"+1"`、`"01"`、`"0001"`，都会得到 1 并通过校验。
协议版本是**冻结的对外契约**，理想情况下只应接受精确的 `"1"`。

**触发条件**：调用方传 `--protocol 01` 或 `--protocol +1`。

**影响**：无实际危害——版本号语义相同，行为完全正确。风险只在于未来若出现
版本 `10`，宽松解析可能掩盖调用方的字符串拼接缺陷。

**严重程度**：Low。

**修复方案（不采用）**：改为精确字符串比较或 `strconv.ParseUint` + 规范化检查。

**修复状态**：暂不修改。理由：这属于「收紧输入校验」，会把今天能跑通的调用方
变成退出码 2。按用户要求「不破坏现有正常行为」，且没有任何实际故障与之关联，
不值得承担兼容性风险。记录备查。

## 需要进一步验证

### [需要进一步验证] V-1 cleanup 的 Canonicalize 与实际删除之间存在 TOCTOU 窗口

**位置**：`internal/cleanup/cleanup.go:446-459` `enumeratePythonCaches()`

**怀疑原因**：枚举阶段用 `filesystem.Canonicalize(path)` 验证「普通目录身份」，
但随后记录的是**路径字符串**：

```go
if _, err := filesystem.Canonicalize(path); err != nil {
    items = append(items, newFailedItem(id, "目录身份不明确"))
    return fs.SkipDir
}
items = append(items, newDeleteItem(
    id, filesystem.DeletePythonCache, path, cleanupOperationID,
))
```

计划构建与计划执行是两个阶段。若在两者之间把该路径换成指向用户数据目录的
junction，按路径删除就会越界——这正是项目红线明确禁止的形状。

**当前证据**：`Canonicalize` 的返回值被丢弃（`_`），说明这里只用它做校验、
不把校验结果传递给删除阶段。删除阶段拿到的只有路径。

**如何验证**：需要读 `filesystem.DeletePythonCache` 的实现，确认它在删除时是否
**自己重新做一次句柄级身份校验**（本项目在 state 写入路径上大量使用
`NtCreateFile` + 句柄复用，很可能删除路径也是如此）。若删除阶段自行校验，
则本条不成立，枚举期的 `Canonicalize` 只是提前反馈；若删除阶段只信路径，
则这是一个真实的 TOCTOU，应当把校验后的句柄或 fileID 一路传下去。

**为什么不直接改**：`filesystem` 包的删除实现是 Windows-only 代码，本环境
**无法运行验证**；且若删除阶段已有校验，改动纯属多余并会增加复杂度。
按用户要求，无法证明的怀疑不动代码。

### [需要进一步验证] V-2 go-git 检出仓库内符号链接时是否可能越出 repo 目录

**位置**：`internal/gitrepo` 的克隆/检出路径（经 go-git v5.19.2 + go-billy v5.9.0）

**怀疑原因**：仓库内容来自远端，若某个提交包含指向 `..\..\` 的符号链接条目，
检出过程写文件时可能落到 repo 之外。项目依赖里有
`cyphar/filepath-securejoin`，说明作者已经意识到这类风险并在**某些**路径上做了
防护，但我没有确认 go-git 的 worktree 写入是否也走了同样的防护。

**当前证据**：仅依赖清单与 `securejoin` 的存在，没有定位到 go-git 检出路径上的
显式约束点。go-git 自身对 symlink 有一定处理，但版本行为需要实测确认。

**如何验证**：构造一个包含恶意 symlink 条目的本地 bare 仓库，用 Runtime 的
`workspace sync` 检出，观察目标文件是否出现在 repo 之外。这需要在 Windows 上
配合真实的 `workspace sync` 流程执行。

**为什么不直接改**：无法在本环境构造并执行该实验；在没有确认防护缺失的情况下
往检出路径插入额外校验，可能破坏合法的仓库内容（AUTO-MAS 仓库本身是否使用
symlink 也需要确认）。

## 第二轮 Review（对本轮改动本身的复查）

第一轮修复完成后，对 `git diff` 逐文件复查，只问三个问题：
改动有没有引入新的并发/阻塞问题、有没有越出「修这个缺陷」的必要范围、
有没有与既有测试所固化的契约冲突。以下两项是复查的产出。

### [High] R-1 M-4 的修复越范围改了错误语义，与两个既有 Windows 测试冲突

**位置**：`internal/lock/worker_windows.go:278-313` `finishThread()`（本轮改动本身）

**当前行为**：M-4 的修复同时做了两件事——

1. 给等待循环加上界 `maxThreadWaitAttempts`（这是 M-4 真正要修的缺陷）；
2. 把 `waitErr = errors.Join(waitErr, ...)` 改成「只保留首个错误」，
   并在 wait 最终成功时执行 `waitErr = nil`。

**问题**：第 2 项不是 M-4 的必要修复，而且它与既有契约相反。
`TestSet_FinishThreadRetriesUntilSignaled`（`worker_internal_test.go:561-631`）
构造的序列是「第 1 次注入错误 → 第 2 次返回 `0x42` 异常结果 → 第 3 次成功」，
然后断言：

```go
if !errors.Is(err, injectedErr) { ... }
if !strings.Contains(err.Error(), "unexpected wait result") { ... }
```

也就是说，既有设计要求**中途出现的异常即使最终 wait 成功也必须上报**——
这是有意的诊断行为（worker 线程等待期间出过怪事，值得让上层看到）。
`testWorkerOperationErrors` 的 `wait-worker-thread` 用例（同文件 728-741 行）
依赖同一语义。我的 `waitErr = nil` 会让第一个断言拿到 `nil`，
「只保留首个错误」会让第二个断言找不到 `unexpected wait result`。
两个测试都会失败。

**触发条件**：在 Windows 上运行 `internal/lock` 的测试即可复现。
本环境从未执行过——`internal/lock` 全部 8 个测试文件都带 `//go:build windows`，
在 Linux 上该包显示 `[no test files]`。**这正是「测试没跑」直接导致的漏网**，
`GOOS=windows go test -c` 的类型检查看不出断言层面的冲突。

**影响**：Windows 侧 CI 直接红；更实质的是丢掉了既有设计刻意保留的诊断信息。

**严重程度**：High（本轮改动引入的回归，且会让 Windows 测试失败）。

**修复方案**：把改动收回到 M-4 的必要范围——**只加上界，保留
`errors.Join` 累积语义，删掉 `waitErr = nil`**。上界为 64，
所以错误链最多 64 项，M-4 描述的「无界增长直到 OOM」同样被消除，
而既有的「异常必须上报」契约不受影响。相应地，M-4 的回归测试
`TestSet_FinishThreadStopsRetryingOnPermanentFailure` 里那条
「错误不得累积」的断言要改为「累积次数等于上界」。

**修复状态**：已修复。已回退错误语义改动，仅保留上界；
`internal/lock` 的既有断言与本轮新增断言现在描述同一套行为。

### [Medium] R-2 H-1 改用自有管道后，`job.Close()` 失败会让排空永久阻塞

**位置**：`internal/uv/runner.go:291-302` `Run()`（本轮改动本身）

**当前行为**：H-1 把 `command.StdoutPipe()` 换成自有的 `os.Pipe()`，
读端因此不再由 `exec.Wait()` 关闭。收口顺序是：

```go
waitErr := command.Wait()
if job != nil {
    close(jobStop)
    <-jobDone
    if err := job.Close(); err != nil {
        waitErr = errors.Join(waitErr, err)   // 只记错，不改变后续流程
    }
}
stdoutValue := <-stdoutResult   // 读端 EOF 才会到达这里
stderrValue := <-stderrResult
```

读端只在**子树里所有写端句柄都关闭后**才看到 EOF。`uv` 自己退出会关掉它那份，
但如果 `uv` 派生的孙进程继承了写端并且活得更久，EOF 就取决于 Job 是否回收整棵树。
`NewJob()` 设置了 `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`（`job_windows.go:35`），
所以正常情况下 `job.Close()` 就是保证 EOF 可达的那一步。

**问题**：`job.Close()` 失败时，代码只把错误 join 进 `waitErr` 就继续往下走，
而此时没有任何其他机制回收子树。孙进程仍然持有写端 →
`<-stdoutResult`（:301）永久阻塞 → `Run()` 永不返回。
修复前 `exec.Wait()` 会关闭读端，读协程立刻以错误失败，
所以 H-1 把「快速失败」换成了「可能永久挂起」——失败模式变差了。

**触发条件**：同时满足三条：`uv` 派生了继承 stdout/stderr 写端且比 `uv` 活得更久的
孙进程；`job.Close()` 返回错误（Job 句柄异常）；该孙进程不自行退出。

**影响**：`uv` 相关操作（`bootstrap`、`dependencies sync/check/rebuild`）挂死，
不发 `result`，Electron 永久等待终态。与 M-5 同形。

**严重程度**：Medium（后果重，但需要「Job 句柄异常」这个前提，
而 Job 句柄由本进程创建并独占）。

**修复方案（不采用）**：三个候选都有实际代价——

1. `job.Close()` 失败后补一次 `job.Terminate(1)`：但 Close 失败通常意味着句柄本身
   有问题，对同一句柄调用 Terminate 大概率同样失败，等于没修。
2. 给排空加超时后直接返回：`stdoutResult` / `stderrResult` 都是 buffered(1)
   且各只有一个发送者，所以读协程不会因为没人接收而永久卡在发送上——
   这一点是安全的。但返回会触发 `defer stdout.Close()`，而读协程可能正阻塞在
   同一个 `*os.File` 的 `ReadFile` 里。Windows 上 `os.Pipe` 建的是非 overlapped
   管道，读是阻塞系统调用；`poll.FD` 的引用计数会推迟真正的 `CloseHandle`
   直到阻塞的读放弃引用，因此该读协程会一直挂着，句柄也一直不释放。
   净效果是把「函数挂死」换成「协程 + 句柄泄漏」，需要在 Windows 上实测
   才能判断哪个更可接受。
3. 改回 `exec` 拥有的管道：等于回退 H-1，重新引入那个更常见的缺陷。

**修复状态**：暂不修改。理由：候选 1 无效，候选 3 是回退，候选 2 的行为
（`os.File.Close()` 与阻塞中的 `ReadFile` 如何交互）**必须在 Windows 上实测确认**，
本环境无法执行——`internal/uv` 在 Linux 上根本不编译。
按用户「无法证明的改动不做」的要求，这里只记录不动代码。
风险可接受的依据：触发前提是本进程独占的 Job 句柄出现异常，
而正常路径（`job.Close()` 成功）已由 `KILL_ON_JOB_CLOSE` 保证 EOF 可达。
**建议**：在 Windows 上按候选 2 做一次实验（构造一个派生长命孙进程的假 uv，
并注入 `job.Close()` 失败），确认 `Close` 与阻塞读的交互后再决定。

## Windows 侧待执行验证清单

本轮所有新增/修改的回归测试都只在本环境通过了 `GOOS=windows go test -c`
的类型检查，**没有任何一条真正运行过**。下面这些命令必须在 Windows +
PowerShell 7 下逐条执行，全部通过才算本轮修复验证完成。
按仓库约定，每条原生命令后检查 `$LASTEXITCODE`。

### 1. 标准门（先跑，确认基线干净）

```powershell
gofmt -l .
go vet ./...
go build ./...
go test ./...
go test -race ./... -count=1
golangci-lint run
```

在本环境中 `golangci-lint` 未安装、`go test ./...` 只覆盖 8 个可编译包中的 28 个
测试文件（131 个的约 21%），所以这一组在 Windows 上是**首次完整执行**。特别注意
`internal/config` 的 4 个 Windows 路径子测试：它们在 Linux 上确定性失败，在
Windows 上应当全部通过；若仍失败，说明是真实缺陷而不是环境差异。

### 2. 本轮修复的定向回归测试

```powershell
# H-1 / M-1：uv 执行器管道所有权与诊断快照截断
go test ./internal/uv/ -run 'TestRunner_CapturesFullOutputOfFastExitingProcess|TestRunner_TruncatesOversizedCaptureWithoutFailing' -v -count=1

# H-1 是概率性竞态，务必加 -race 并重复
go test ./internal/uv/ -run TestRunner_CapturesFullOutputOfFastExitingProcess -race -count=20

# M-3：已落盘成品（Published=true）不得触发镜像轮换
go test ./internal/uv/ -run TestBootstrap_PublishedDownloadFailureIsTreatedAsSuccess -v -count=1

# H-6：版本源失败映射 INTERNAL_ERROR，超时映射取消
go test ./internal/cli/ -run 'TestVersionCommand_SourceErrorMapsToInternalError|TestVersionCommand_DeadlineExceededSourceMapsToOperationCancelled' -v -count=1

# M-6：package-index 覆盖在任何副作用之前被拒
go test ./internal/cli/ -run 'TestM5CommandsRejectPackageIndexOverrideBeforeSideEffects|TestBootstrapCommand_OrderAndStates' -v -count=1

# M-2 / L-2：进度探测失败关闭响应体；(0, nil) 读可被取消
go test ./internal/mirror/ -run 'TestDownloader_UnknownSizeProgressProbeFailureClosesBody|TestPumpBody_ZeroReadRespectsCancellation' -v -count=1

# M-5：转发 goroutine 静默退出时 cancel 仍能收口
go test ./internal/backend/ -run TestBackend_CancelFinishesWhenControlForwarderStoppedSilently -v -race -count=1

# M-4 / R-1：等待重试有上界，且既有的「异常必须上报」语义未被破坏
# 这三条必须一起跑——R-1 正是「只跑新测试、不跑既有测试」造成的漏网
go test ./internal/lock/ -run 'TestSet_FinishThreadStopsRetryingOnPermanentFailure|TestSet_FinishThreadRetriesUntilSignaled' -v -count=1
go test ./internal/lock/ -v -count=1
```

### 3. 并发与重复稳定性

`internal/lock`、`internal/backend`、`internal/process`、`internal/uv` 的
Windows 专属测试在本环境一次都没跑过，建议重复执行以暴露调度相关的不确定性：

```powershell
go test ./internal/lock/ ./internal/backend/ ./internal/process/ ./internal/uv/ -race -count=10
```

### 4. 需要人工实验的两项

- **R-2**：构造一个派生长命孙进程（继承 stdout 写端）的假 uv，并注入
  `job.Close()` 失败，观察 `Run()` 是否挂死在 `<-stdoutResult`；
  再验证「加超时后返回」是否会让读协程与句柄泄漏。
- **V-1 / V-2**：见各条目的「如何验证」小节，都需要真实文件系统与
  真实 `workspace sync` 流程。

## 文档整理（本轮附带）

Review 之外按要求精简了 `README.md`，只保留「这是什么 / 怎么构建 / 怎么用」：
Quick Start、命令一览、managed 与 development 两种模式、Electron 调用所需的最小
NDJSON 与 stdin 示例。以下内容**移出 README 并指向已有权威文档，不是删除**——
每一处都先确认目标文档已完整覆盖后才裁剪：

| 移出内容 | 原 README 位置 | 现在的权威位置 |
| --- | --- | --- |
| 全局选项完整表（默认值、各命令接受的镜像类型） | `## 全局选项` 表格 | `doc/架构设计.md:437-452` |
| 退出码完整对照表 | `### 退出码` 表格 | `doc/架构设计.md:713-717`，错误码分域表见 `:736`、`:763`、`:777`、`:791` |
| stdin 控制命令完整语义（`commandId` 回显、`INVALID_CONTROL_COMMAND`） | `### stdin 控制命令` | `doc/架构设计.md:699-711` |
| 带 `$LASTEXITCODE` 检查的完整验证门、race detector 的 PATH 前置条件与已知限制 | `### 标准验证` | `AGENTS.md` 第 5 节（5.2、5.3） |
| Sentry 净化白名单与零网络门禁的具体要求 | `## 错误观测` | `doc/代码审查清单.md:46-74` |

README 保留了每条被移出内容的**行为要点**而非细节，避免调用方必须跳文档才能
避免踩坑：退出码只能粗粒度分类、精确原因读 `result.code`；`--offline` 与
`--mirror`/`--mirror-only` 互斥；关闭监督进程用 stdin `shutdown` 而不是按进程名
杀 Python；每条原生命令后必须检查 `$LASTEXITCODE`。

同时把本文件登记进 `doc/README.md` 的「按用途浏览」——此前它不在任何索引里，
从文档入口无法发现。没有新建目录，没有创建 `-v2`/`-new`/`-final` 变体。
