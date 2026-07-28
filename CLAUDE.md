# CLAUDE.md

## 首要指令

**本仓库的全部工作规范集中在 [AGENTS.md](AGENTS.md)。开始任何任务前先完整读一遍，并在整个会话中遵守。**

本文件只补充 Claude Code 特有的使用约定，不重复 AGENTS.md 的内容；两者冲突时以 AGENTS.md 为准。

## 快速定位

| 我要做什么 | 先看哪里 |
| --- | --- |
| 了解项目、当前进度、下一步做什么 | [AGENTS.md](AGENTS.md) 第 1~2 节、[doc/任务拆分.md](doc/任务拆分.md) |
| 确认对外契约（事件/错误码/stage/state/环境变量/目录） | [doc/架构设计.md](doc/架构设计.md)、[doc/契约补充-v1.md](doc/契约补充-v1.md) |
| 跑构建 / 测试 / 格式检查 | [AGENTS.md](AGENTS.md) 第 5 节（⚠️ Go 不在 PATH，必须用绝对路径） |
| 写 Go 代码（命名/错误/并发/接口/注释语言） | [AGENTS.md](AGENTS.md) 第 8 节 |
| 提交代码 | [AGENTS.md](AGENTS.md) 第 6~7 节 |
| 提交前自查 | [doc/代码审查清单.md](doc/代码审查清单.md)、[AGENTS.md](AGENTS.md) 第 10 节红线清单 |

## 会话约定

1. **先确认任务编号。** 实现类工作必须对应 `doc/任务拆分.md` 里的某个 `T<里程碑>.<序号>`；
   清单里没有的需求，先与用户确认并补进文档，再动手。
2. **走既有四段式流程。** 设计文档 → 实现计划 → 严格 TDD 逐任务实现 → 每任务提交后审查。
   非平凡任务先进入 plan mode 或产出 `doc/设计-T*.md`，取得确认后再写代码。
3. **证据优先。** 声称“测试通过 / 构建成功”前必须实际执行并贴出输出；
   `-race` 在本机不可用（`CGO_ENABLED=0` 且无 C 编译器），不得声称 race 已验证。
4. **推送需授权。** `git push`、创建远端仓库/Release、任何把仓库内容发往外部服务的操作，
   都必须先得到用户明确许可。
5. **语言约定。** 与用户对话、`doc/` 文档、**Go 注释（含 doc comment）**一律中文；
   标识符、Go error 字符串、提交信息用英文。详见 [AGENTS.md](AGENTS.md) 8.1。
6. **命令用 PowerShell 7。** 每条原生命令后检查 `$LASTEXITCODE`；不要假设失败会自动中断脚本。

## 可用技能

本仓库的工作方式与以下 skill 高度匹配，遇到对应场景优先调用：

- `brainstorming` — 在设计新功能、修改行为之前澄清意图与边界；
- `writing-plans` / `executing-plans` — 产出与执行 `doc/计划-T*.md`；
- `test-driven-development` — 本仓库强制 TDD，实现前先写失败测试；
- `systematic-debugging` — 出现 bug、测试失败、异常行为时，先定位再改；
- `verification-before-completion` — 宣称完成、提交或合并前的验证门；
- `requesting-code-review` / `receiving-code-review` — 每个 Task 提交后的独立审查环节；
- `using-git-worktrees` — 特性开发在 `.worktrees/<name>` 下隔离进行。
