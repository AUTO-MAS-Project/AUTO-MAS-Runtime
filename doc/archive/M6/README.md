# M6 实现说明

M6 实现 Windows 首版的后端进程监督：Runtime 通过挂起创建和 Job Object 原子纳管 uv 根进程，
转发并持久化 Python 双流日志，以固定 loopback health/close 接口确认身份与收口事实，并提供
managed/development 两种策略、stdin 控制、一次自动重启和确定性的进程树清理。

权威依据仍为 `doc/契约补充-v1.md`、`doc/架构设计.md` 和 `doc/任务拆分.md`。本目录记录
已经完成实现的设计背景；若与权威文档冲突，以权威文档为准。

## 文档索引

- [`设计-M6-后端监督.md`](./设计-M6-后端监督.md)：组件边界、Windows 进程模型、健康与身份、
  生命周期、控制/重启、development 安全边界以及测试验收策略。

已执行的 T6.1～T6.7 实施计划不在工作树长期保留，可通过 Git 历史按任务编号检索。

## 本轮冻结决策

- 长驻 uv 必须先以 `CREATE_SUSPENDED | CREATE_NO_WINDOW` 创建，在恢复唯一初始线程前加入启用
  `KILL_ON_JOB_CLOSE` 的 Job；无法证明 Job 空树时失败关闭。
- health 和 close 固定使用 `127.0.0.1:36163`，禁用代理、动态端口和 `localhost`；managed 校验
  版本/Commit/Job 身份，development 只校验协议并消费显式 repo 与既有 `.venv`。
- `backend supervise` 的状态、日志、warning、error 与 result 通过唯一协议出口保持顺序；控制命令
  使用有界 FIFO 和 first-wins terminal，首次运行期异常退出最多重启一次。
- 根进程退出不等于管道或 Job 空树。清理在发现后代或 Snapshot 竞态时先终止树，再等待双流与
  `WaitEmpty`；Runtime 异常终止依赖 Job handle 关闭回收全部后代。
- Windows E2E 默认进入普通 `go test ./...`，以跨进程命名 Mutex 串行固定端口，并逐代持有
  uv/Python/孙进程同步句柄，验证 transaction、backend Mutex、日志、端口和临时状态全部收口。
