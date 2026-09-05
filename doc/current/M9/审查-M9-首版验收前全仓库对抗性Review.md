# M9 首版验收前全仓库对抗性 Review

日期：2026-08-13

审查基线：`18189e3`（`main`）

远端关系：审查时 `main...origin/main [ahead 2]`；远端 `origin/main` 为 `26eb49f`。

## 1. 结论

本轮按 `契约补充-v1.md > 架构设计.md > 任务拆分.md` 的优先级，对当前 Runtime 全仓生产代码、
测试、发布工作流和仍生效的设计边界做只读对抗性审查。没有修改生产代码、测试、既有权威文档
或任务进度，也没有提交、push、创建 tag/Release 或访问真实 Sentry。

审查结论：

- Critical：0；
- Important：1，发布验收前必须修复；
- Minor：0；
- 已知剩余风险：1，不计入当前契约缺陷；
- 未完成验收：M9 T9.1～T9.4、M12 T12.6，均不能由本轮静态审查或自动化门替代。

当前代码不能按“全仓门禁全绿”直接判定为首版可发布。原因不是现有测试失败，而是已确认的
Runtime-only 遥测配置越过进程边界，以及任务清单仍存在明确未完成的真实联调和远端验收。

## 2. 审查范围与方法

### 2.1 范围

- 164 个生产 Go 文件、135 个 Go 测试文件；
- `cmd/auto-mas-runtime` 与 `internal/protocol`、`cli`、`config`、`state`、`lock`、
  `filesystem`、`mirror`、`gitrepo`、`uv`、`process`、`health`、`backend`、`telemetry`、
  `logging`、`doctor`、`cleanup`、`version`；
- `.github/workflows/ci.yml`、`.github/workflows/release.yml`；
- 当前权威契约、代码审查清单、M4～M7 历史设计/审查记录、M9 与 M12 当前任务边界。

### 2.2 对抗面

审查不是只看正常路径，而是逐域检查：

1. NDJSON 首尾事件、stdout 所有权、错误四元组、冻结 stage/state/code；
2. Windows 路径身份、Junction/硬链接、inspect-to-use 竞态、原子替换、崩溃恢复；
3. 锁、事务所有权、提交点前后错误优先级、取消后的独立收口；
4. uv/Python 环境隔离、参数数组、Job Object、进程树与日志管道；
5. backend managed/development 身份、健康检查、控制命令、重启与退出清理；
6. Sentry 配置前置门、白名单净化、失败静默、500 ms 收口、跨包和子进程边界；
7. Release 凭据注入、不可覆盖发布、资产集合、禁遥测 smoke；
8. 测试是否真实运行、是否存在 `[no tests to run]`、race 与当前任务验收是否被混淆。

## 3. 必须修复

### Important-1：环境覆盖形式的 Sentry 配置泄漏给 uv/Python 子进程

#### 发现

`internal/telemetry/config.go:14-17` 定义了四个 Runtime 遥测配置键：

- `AUTO_MAS_TELEMETRY`；
- `AUTO_MAS_SENTRY_DSN`；
- `AUTO_MAS_SENTRY_ENVIRONMENT`；
- `AUTO_MAS_SENTRY_RELEASE`。

`internal/uv/runner.go:464-474` 从 `os.Environ()` 继承宿主环境，只剔除 `UV_*`、受控 override
和 `canonicalSupervisionEnvironmentKey` 识别的键。后者在
`internal/uv/runner.go:493-505` 仅包含 `AUTO_MAS_UV_EXE`、协议/版本/Commit 和
`AUTO_MAS_SUPERVISED`，没有包含上述四个 M12 键。

因此：

- 一次性 uv 调用在 `internal/uv/runner.go:147-149` 把四个键交给 uv；
- 长驻调用在 `internal/uv/managed.go:80-85` 把相同环境交给受管 uv/Python；
- managed 与 development 后端最终都消费这条环境构造路径；
- 环境覆盖形式的 DSN 会被 Python 后端、依赖构建 hook 或插件读取。发布构建通过 ldflags
  注入的 `BuildSentryDSN` 不在 `os.Environ()` 中，本缺陷不涉及该默认值。

#### 契约冲突

`doc/current/M12/设计-T12.1-遥测与错误观测.md:48-49` 要求配置只在
`internal/telemetry` 构造边界解析一次；同文 `64-71` 把 DSN 纳入受控凭据边界；
`doc/架构设计.md:1677-1683` 要求 `internal/telemetry` 是唯一观测接入层，凭据不得进入日志、协议
或其他实现边界；`1689-1690` 还要求 offline/显式关闭在任何网络副作用前短路。

当前实现虽然没有把 DSN 写入 Runtime 的 stdout/stderr，但把它交给了不属于 telemetry adapter
的第三方子进程，违反“集中解析一次”和“唯一接入层”的边界。`--offline` 或
`AUTO_MAS_TELEMETRY=disabled` 只会关闭 Runtime 自己的 provider，不能撤回已经继承给子进程的
配置；子进程是否实际发送网络请求取决于其代码，因此网络发送是条件性风险，不作为已发生事实。

#### 风险

- Python 后端、依赖安装脚本或插件可读取 Sentry ingest DSN；
- 非预期代码可伪造/污染事件、消耗配额，或把 DSN 写入自己的日志；
- Runtime 的禁用与离线语义不能覆盖已泄漏到子进程的同名配置；
- 测试目前只证明宿主 `UV_*` 和五个监督键被清理，没有证明 Runtime-only 遥测键被清理。

DSN 不是 Sentry auth token，不能用于管理项目；但项目权威文档仍把它作为凭据处理，且泄漏面包含
依赖构建与插件边界，因此定级为 Important，而不是 Critical。

#### 建议修复方向

1. 先按仓库流程在 `doc/任务拆分.md` 登记独立 T12.x 修复任务和变更记录；如需调整“Runtime-only
   环境变量不得下传”的契约表述，先更新设计，再写代码。
2. 在 uv 环境构造的唯一入口按 Windows 大小写不敏感规则剔除全部四个遥测键；同时拒绝调用方通过
   `RunOptions.Environment` 重新注入这些保留键。
3. 不把这些键重新注入 backend；backend 只应获得既有身份、监督和受控 uv 环境。
4. 保留 PATH 与其他确有必要的普通宿主环境，避免用全量 allowlist 引入无关兼容性回归。

#### 必须先红后绿的测试

- `TestRunner_ScrubsTelemetryEnvironment`：fake uv 回显完整环境，覆盖四个键和大小写变体；
- `TestManagedRunner_ScrubsTelemetryEnvironment`：`StartManaged` 子进程同样不可见；
- `RunOptions.Environment` 试图注入四个保留键时仍不可见；
- managed/development backend 组件测试分别断言子进程不可见；
- `--offline`、`AUTO_MAS_TELEMETRY=disabled` 和正常启用三种配置均不下传；
- 反例断言 PATH、必要普通变量和既有监督身份仍正确继承。

验收时除定向测试外，必须重新跑全仓标准门、`internal/protocol -count=100` 和完整 race；再从当前
源码构建新 EXE，以子进程回显环境的黑盒夹具验证 ordinary uv 与 managed/development 两条路径。

## 4. 已知剩余风险（不计当前 finding）

### Info-1：一次性 UVRunner 仍存在 `Start → Assign Job` 窗口

`internal/uv/runner.go:175-184` 先启动进程，再创建 Job；`207-219` 才执行 Assign。理论上，uv 可在
Assign 前快速派生后代，随后取消或输出溢出只终止已纳入 Job 的进程树，极窄窗口中的先行后代可能
继续写受管目录或占用句柄。

本轮不把它列为当前缺陷，原因是权威历史范围已明确接受该边界：

- `doc/archive/M5/设计-M5-uv-python-依赖.md:43-46` 明确把 `CREATE_SUSPENDED` 启动原子性留给 M6；
- `doc/archive/M6/设计-M6-后端监督.md` 把强原子性落在长驻 `StartManaged` 路径；
- `doc/架构设计.md:1352-1357` 也把挂起纳管表述为长驻 `uv run`，一次性 install/sync 执行完即退出。

若产品以后要求一次性 uv 也具备“派生前必已纳管”的强保证，应先扩展设计和任务范围，再复用
`process.StartManaged` 的 suspended starter，或提供专用的一次性挂起启动 API。验证必须注入
`Start → Assign` barrier，让 helper 在窗口内派生持久子进程；不能用“尽快 spawn + 多轮重跑”的
概率测试冒充确定性证据。

## 5. 未完成验收与发布阻断

以下是任务清单中的真实缺口，不是本轮发现的代码 defect，但在完成前不能宣称首版验收通过：

1. M9 T9.1 尚未完成：真实 AUTO-MAS development 后端的启动、连续健康就绪、优雅关闭、日志转发
   和 Job 树清理没有当前完成记录；
2. M9 T9.2 尚未完成：全新临时根的真实 managed bootstrap/supervise、升级和显式降级未验收；
3. M9 T9.3 尚未完成：Electron 双链路灰度和桌面 E2E 未验收；
4. M9 T9.4 尚未完成：`doc/首版验收记录.md` 尚未形成逐条证据；
5. M12 T12.6 尚未完成：Repository secret、远端发布以及专用 HTTPS Sentry 项目端到端验收仍待
   明确授权。本轮没有读取 secret、执行 `gh`、push tag 或触发 Release，不能把静态 workflow
   审查写成远端通过证据。

## 6. 已执行验证

### 6.1 标准门

在当前 `18189e3` 工作树执行：

- `gofmt -l .`：无未格式化文件，exit 0；
- `go vet ./...`：exit 0；
- `go build -buildvcs=false ./...`：exit 0；
- `go test ./... -count=1`：全包通过，exit 0；
- `git diff --check`：exit 0。

### 6.2 Race

按 `AGENTS.md` 5.3 将实际 GCC 目录提升到当前 PowerShell 7 会话 PATH 首位，并使用独立 GOCACHE：

- 受管沙箱内首次运行复现已知 `runtime/cgo: ... cgo.exe: exit status 2`，不计为代码失败，也不计为
  race 通过证据；
- 获批在沙箱外重跑同一 `go test -race ./... -count=1`，全包通过，exit 0。

### 6.3 静态边界扫描

- stdout 所有权扫描：生产路径没有发现 `internal` 直接持有 `os.Stdout`；命中项均为入口、显式传入
  writer 的 renderer/诊断路径、测试或夹具；
- telemetry 依赖扫描：`sentry-go` 只由 `internal/telemetry` adapter 直接导入；没有当前
  Umami/PostHog 实现；
- 外部进程扫描：生产 uv 执行仍使用参数数组；没有发现业务包直接调用系统 Python/pip 或拼 Shell；
- `panic`/`os.Exit` 扫描：库生产代码没有新增直接终止进程路径，`os.Exit` 仍只在唯一 main 入口；
- 测试覆盖检索：uv/backend 测试中没有四个 M12 环境键的清理断言，这与 Important-1 的缺口一致。

## 7. 反证与排除项

为避免把“可疑代码”直接抬高为缺陷，本轮明确排除：

1. 一次性 UVRunner 的 Job 分配窗口：代码事实成立，但当前设计明确未承诺强原子性，故只列 Info；
2. `Observer.Close` 对任意不遵守 context 的注入 provider 理论上可能留下收口 goroutine，但生产仅有
   Sentry provider，当前实现以有界 `FlushWithContext` 收口，未找到可触发的生产证据，故不报 finding；
3. Release 使用 ldflags 注入 DSN：静态审查未发现把 secret echo 到 workflow 日志、协议或发布说明；
   但真实 secret masking、成品白名单和自建 endpoint 仍属于 T12.6 远端验收，不能据此宣称通过；
4. M9/T12.6 未完成项：保持“验收缺口”分类，不伪装成代码错误；
5. 全仓标准门与 race 全绿：只能证明当前自动化覆盖范围，不反证 Important-1，也不能代替真实
   Electron/后端/发布黑盒。

## 8. 建议处理顺序

1. 先登记 Important-1 的任务和契约边界，严格 TDD 修复；
2. 完成全仓标准门、协议 100 轮、完整 race 与新 EXE 子进程环境黑盒；
3. 完成 M9 T9.1～T9.4 的真实联调和首版核对；
4. 对 M12 T12.6 单独请求远端授权，报告将要推送的精确 ref 与实际 divergence 后，再做 secret、
   Release 和专用 Sentry 项目验收；
5. 只有 Important 清零且上述任务均有当次证据，才将首版验收结论改为通过。

## 9. Git 与外发边界

- 本轮只新增本审查文档；
- 未修改代码、测试、既有文档或任务状态；
- 未 stage、commit、push；
- 未调用 `gh`、未创建 PR/tag/Release，未向外部服务发送仓库内容。
