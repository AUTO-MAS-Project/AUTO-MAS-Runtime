# M5 实现说明

M5 实现 `internal/uv` 的 uv bootstrap、统一执行器、受管 Python、锁定依赖环境以及
`bootstrap`/`environment`/`dependencies`/`repair` 编排。首版运行目标是 Windows；高层
请求、错误和事件不绑定 Windows API，平台相关的发行文件名、可执行文件名和受控文件系统
操作保持在已有 `config`/`filesystem` 边界内，为 M11 的 Linux/macOS 适配留下替换点。

权威依据仍为 `doc/契约补充-v1.md`、`doc/架构设计.md` 和 `doc/任务拆分.md`。本目录记录
本轮已完成实现方案；若与权威文档冲突，以权威文档为准。

## 文档索引

- [`设计-M5-uv-Python-依赖.md`](./设计-M5-uv-Python-依赖.md)：原始实现设计；
- [`审查-M5-对抗性复审.md`](./审查-M5-对抗性复审.md)：`e67a548` 时点的历史审查快照，
  其中完成结论已被后续完整复审撤回；
- [`设计-M5-复审修复.md`](./设计-M5-复审修复.md)：最终生效的复审修复边界与验收记录。
- [`设计-T5.9-Windows-uv发布回归.md`](./设计-T5.9-Windows-uv发布回归.md)：真实打包 EXE
  暴露的 Windows uv 发布、失败收口与只读检查回归的修复设计；
- [`审查-T5.9-Windows-uv发布回归.md`](./审查-T5.9-Windows-uv发布回归.md)：T5.9 红绿灯、
  官方资产、race、最终 EXE 黑盒与独立审查证据。

## 本轮冻结决策

- uv 版本：`0.12.3`。
- Windows x64 制品：`uv-x86_64-pc-windows-msvc.zip`。
- SHA-256：
  `b23350c79e8ad0192b8124af13a0f17e8d4e4549524785e1aef389ae5a06990e`。
- 下载源：复用 `internal/mirror` 的 `KindUV` source rotation，官方源是
  `https://github.com/astral-sh/uv/releases/download`，镜像源保持现有 catalog。
- 下载器只发布经过 `.part`、HTTP `Content-Length`/实际字节数和 SHA-256 校验的缓存文件；
  release 资产大小由响应头冻结到本次传输并再次由实际读取字节数核对，不把易漂移的十进制
  “MB”展示值写入协议常量。
- 禁止在线安装脚本、`uv self update`、Shell 字符串和系统 Python 回退。

## 平台边界

`uv` 的版本、校验和、命令参数、环境变量和协议错误是跨平台的高层契约。当前 Windows
实现使用既有 `config.Layout.UVExecutable`、`filesystem.Operator` 和 Windows Job/Mutex
基础设施；未来平台实现应新增平台适配器，不改变 M5 的服务请求、结构化错误或 NDJSON
生命周期。
