# M2 基础设施归档

M2 已完成。本目录保存 T2.1～T2.7、T2.9 与 T2.10 的最终设计，用于解释 `config`、`logging`、
`state`、`lock`、`filesystem` 和 `mirror` 的安全边界。

精确实施步骤和中间红绿灯不再长期维护；需要追溯时按任务编号查看 Git 历史。
当前行为以生产代码、测试和根目录权威文档为准。

## 设计

- [T2.1 config 与目录布局](./设计-T2.1-config与目录布局.md)
- [T2.2 logging](./设计-T2.2-logging.md)
- [T2.3 state 持久化](./设计-T2.3-state持久化.md)
- [T2.4 Windows 命名 Mutex](./设计-T2.4-Windows命名Mutex.md)
- [T2.5 filesystem 安全操作](./设计-T2.5-filesystem安全操作.md)
- [T2.6 mirror 源管理](./设计-T2.6-mirror源管理.md)
- [T2.7 HTTP 下载器](./设计-T2.7-HTTP下载器.md)
- [T2.9 hosted Windows 路径语义修复](./设计-T2.9-hosted-Windows路径语义修复.md)
- [T2.10 uv 镜像源扩充](./设计-T2.10-uv镜像源扩充.md)
