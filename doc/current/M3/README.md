# M3 CLI 框架与基础命令

状态：尚未开始。

本阶段包含 `T3.1`～`T3.4`：CLI 框架与全局参数、`version`、`doctor` 和 `cleanup`。
具体范围和验收以 [任务拆分](../../任务拆分.md) 为准。

开始某个任务时，在本目录创建：

- `设计-T3.x-<主题>.md`
- `计划-T3.x-<主题>.md`

M3 必须优先复用成熟能力：CLI 默认选用 Cobra；业务包不得解析参数或直接写 stdout；
基础设施能力通过现有 `config`、`state`、`lock`、`filesystem`、`logging` 和 `protocol`
边界接入，不复制 M2 的内部实现。
