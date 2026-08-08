# M5 设计：uv、Python 与依赖环境

## 1. 目标与不负责

Runtime 负责在受管根目录内取得并验证固定 uv、用 uv 安装精确 Python、按现有锁文件建立
主项目 venv，并把这些步骤编排为可取消、可恢复、可观察的 Runtime 操作。Runtime 不负责
Python 插件依赖、业务 HTTP/WebSocket、后端进程监督或修改 `uv.lock`。

一次 M5 操作只有一个 `operationId` 和一个 `protocol.Emitter`。业务包只调用类型化事件
出口；stdout/stderr 只能作为 uv 行日志进入日志能力，不能写 Runtime stdout，也不能以自然
语言内容驱动业务分支。

## 2. 组件关系

```text
CLI command
  └─ environment/bootstrap/repair service
       ├─ Bootstrapper      顺序与状态编排
       ├─ Bootstrap          固定版本下载、校验、解压、原子发布
       ├─ Runner             唯一 uv exec.CommandContext 封装
       ├─ Python             .python-version + requires-python + uv install
       └─ Dependencies       uv.lock + uv sync --locked + venv repair
```

依赖方向保持单向：`cli -> uv`，`uv -> config/filesystem/mirror/state/protocol`；uv 不反向
依赖 cli，不启动后端，不直接拥有 stdout。Git 工作区同步由 M4 的 service 提供给
Bootstrapper，M5 只消费其最终 revision。

## 3. uv bootstrap

1. 检查 `runtime/tools/uv/0.12.3/uv.exe` 是否存在，并运行 `uv.exe --version`；输出必须
   与固定版本严格一致，缓存命中不得重新下载。
2. 未命中时按 `mirror.KindUV` 计划下载 zip 到 `runtime/cache/downloads` 的 `.part`，由
   现有 downloader 完成 HTTPS、响应长度/实际读取长度、SHA-256 和 no-replace 发布。
3. 在 `runtime/cache/build/uv/0.12.3/<operationId>` 解压，仅接受预期 `uv.exe` 文件；禁止
   路径穿越、重复发布和覆盖已存在版本目录。
4. 使用 `filesystem.RenameUVStagingToVersion` 把暂存目录原子移动到版本目录。任何发布
   事实发生后再失败都不得报告成功，且保留可诊断状态。
5. 再次运行 `uv.exe --version`，失败返回 `UV_VERSION_MISMATCH`；只有此检查通过才返回
   可用执行器。

ZIP 解压器只允许 regular file、拒绝绝对名和 `..` 逃逸，限制条目数/解压总量，并在暂存
目录内建立输出；它不能覆盖用户数据、插件或安装根。M5 在 Windows 上要求每次 uv
执行都进入 Job Object；其他平台在对应进程树等价实现交付前失败关闭，不降级为无监督
执行。M5 只保证 uv 命令的取消与异常收口；Windows `CREATE_SUSPENDED` 先纳管再恢复
的启动原子性属于 M6，M5 不把该专项验收冒充为已完成。

项目输入 `.python-version`、`pyproject.toml` 和 `uv.lock` 只接受受控 regular file，
读取有明确大小上限；符号链接、目录和超限文件按契约失败关闭。Windows Junction、
文件身份锚定和发布后进程树的挂起纳管属于 `filesystem`/M6 的 Windows 专项门，M5
只通过现有受控目录服务和 `UVRunner` 接口消费，不把这些尚未实现的能力伪装成跨平台完成。

## 4. UVRunner

Runner 的输入是已分割的参数数组。它统一设置：

```text
UV_PYTHON_INSTALL_DIR=<managed python dir>
UV_CACHE_DIR=<runtime cache/uv>
UV_PROJECT_ENVIRONMENT=<managed venv or development repo/.venv>
UV_MANAGED_PYTHON=1
UV_NO_MODIFY_PATH=1
UV_PYTHON_INSTALL_BIN=0
UV_COLOR=never
UV_NO_PROGRESS=1
```

镜像策略通过结构化环境/参数传入；Runner 不拼接 Shell，不调用 `python.exe`、`pip` 或
`.venv` 解释器。stdout/stderr 使用独立管道逐行转发并同时保留诊断摘要；上下文取消和
超时直接终止子进程。退出码、取消和启动失败均转换为包含 `Code`、`Stage`、退出码和
安全细节的结构化错误。

`--version` 是唯一允许把固定命令的受限结果校验为版本 token 的路径；普通 sync/install
流程只看退出码和文件事实，绝不解析 uv 自然语言。

## 5. Python 与依赖

- `.python-version` 必须存在、只有一个精确 `major.minor.patch` token，并拒绝系统 Python。
- `[project]` 的 `requires-python` 必须可解析，精确版本必须满足全部上下界；缺失或不支持
  的 Python 分别映射到稳定错误码。
- 安装命令固定为：
  `uv python install <exact> --managed-python --install-dir <dir> --no-bin --no-registry`。
- 依赖同步前要求 `uv.lock` 为 regular file；先用只读 locked 检查证明锁文件未过期，再执行：
  `uv sync --project <repo> --python <exact> --locked --no-default-groups --no-install-workspace`。
- `sources` 和 `uv.lock` 不得被 Runtime 改写。`rebuild` 只经 T2.5 受控删除删除 managed
  venv，然后重新执行 locked sync；不删除仓库、用户数据、日志或插件。
- sync 成功后以 active revision 原子写 `ready_to_start`；失败以 `operation_failed` 写
  `environment_broken`，保留 `lastSuccessful` 与完整工具/阶段/日志字段。

## 6. 编排与恢复

`bootstrap --version` 固定顺序：

```text
preparing_uv → syncing_repository → preparing_python → syncing_environment → ready_to_start
```

每个瞬态只发 progress/state 事件和操作日志；持久化只允许 `environment_broken` 与
`ready_to_start`。不启动 backend。重复执行从已存在的受管事实继续，workspace no-op、uv
缓存命中、Python 已满足和 locked sync 均应幂等。

`runtime-state/mutation.json` 是单槽事务文件，不能让 M4 workspace sync 与 M5 环境阶段
同时持有。bootstrap 使用同一个 `operationId` 做两次有界交接：uv 阶段先记录并收口事务，
workspace sync 在其自己的 M4 mutation/update 事务内完成；workspace 返回有效 revision 后，
M5 再接管 mutation 槽记录 Python/依赖阶段，成功或失败都在独立 cleanup context 中收口。
这样既覆盖 uv 与环境阶段的崩溃现场，也不破坏 M4 对仓库 swap 的恢复所有权。

`repair` 的边界是环境：体检 uv/Python、必要时重下重验 uv、重装受管 Python、重建 managed
venv。它不得 Git sync，不得改 `uv.lock`，不得访问用户数据和插件目录。

## 7. 测试与审查门

每个 Task 先按计划中的固定测试名确认红灯，再实现最小绿灯；随后检查 `git diff`、暂存
范围和 `git diff --check`。T5.2 及其消费者额外执行协议包 `-count=100` 与完整 race
detector。每个 Task 必须有独立审查者分别给出规格合规性、代码质量结论；Critical/Important
修复后复审清零才能进入下一项。最终再运行全仓标准门、静态 stdout/uv 调用扫描和一轮
详细对抗性 CR。
