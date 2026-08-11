# M7 实现说明

M7 建立了 Windows x64 的 GitHub Release 流水线：受保护的 `v*` tag 触发
`package → publish → smoke` 三个 job，构建期注入版本、Commit 和 UTC 构建时间，发布未签名
zip 与 SHA-256 校验资产，并从真实 Release 下载产物执行严格 NDJSON 冒烟。

## 文档索引

- [`设计-M7-GitHub-CI-CD.md`](./设计-M7-GitHub-CI-CD.md)：版本来源、workflow 拓扑、资产布局、
  不可变 tag 门禁和验收边界。

已执行的 T7.1～T7.3 实施计划不在工作树长期保留，可通过 Git 历史按任务编号检索。

## 发布验收事实

- 测试 prerelease `v0.1.0-beta.2` 的 annotated tag 在验收时 peel 到提交
  `9d7cb9ae616eb1d93b76fa96e61afe72e837e33c`；Release workflow run `31407585577` 的
  `package`、`publish`、`smoke` job 全部成功，check-run annotations 为 0。
- Release 资产为 `auto-mas-runtime-v0.1.0-beta.2-windows-x64.zip` 与 `SHA256SUMS.txt`；
  独立下载的 zip SHA-256 为 `c079bdf3b74a33d26921604b1d6fc940ee9a1b818e7e65080791f9c38cd02d4f`，
  与校验文件及 GitHub API digest 一致。
- zip 根目录恰含 `auto-mas-runtime.exe`、`LICENSE`、`README.md`；二进制为 Windows x64、
  Authenticode 状态为 `NotSigned`；`version` 和 `doctor --output ndjson` 均以退出码 0 完成，
  严格 JSON 事件序列均为唯一首 `hello` 和末 `result`。
- T7.4 按没有已确认的 CNB 仓库、凭据和外发授权跳过；T7.5 按 D3 保持延后，见根文档的
  `D-open-7`。

权威进度和最终提交证据仍以 `doc/任务拆分.md`、`AGENTS.md` 和架构文档为准。
