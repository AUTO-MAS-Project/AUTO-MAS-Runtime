# M7 设计：GitHub CI/CD 发布

## 1. 目标与边界

M7 为 Runtime 建立可重复的 Windows x64 发布流水线。受保护的 `v*` tag 触发一次构建，
流水线在 `windows-latest` 上运行仓库测试，使用固定的 Go 模块路径把 tag、提交和构建时间
注入 `internal/version`，生成未签名的 `auto-mas-runtime.exe`，再发布带校验和的 zip 资产。

Runtime 本身不执行自更新、不签名、不上传用户数据，也不把 GitHub Actions 逻辑迁入 Runtime。
本仓库 workflow 只发布 `auto-mas-runtime.exe`；AUTO-MAS 后端仓库的
`release/<version>` 分支与同名 tag 创建、锁定和 `uv lock --check` 属于 AUTO-MAS 侧
`TODO-CI-2/3`，不由本仓库 M7 创建或校验。
SignPath 接入继续遵守决策 D3，留在 T7.5；CNB 镜像同步在没有目标仓库和凭据前不启用。

## 2. 版本与构建事实

发布版本只接受以 `v` 开头、可安全映射到仓库发布分支的 ASCII 标识符。workflow 在构建前拒绝
路径分隔符、空白、重复点、`@{`、末尾点和 `.lock` 后缀，避免 tag 被当作路径或 PowerShell
表达式。构建使用以下三个唯一注入点：

```text
github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version.Version
github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version.Commit
github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version.BuildDate
```

未经过 linker 注入的本地开发构建继续显示 `dev`，空提交和构建时间；`version` 命令与
`hello.runtimeVersion` 仍消费同一个 `version.Load` 来源。

构建 Commit 从 checkout 后的 `HEAD^{commit}` 解析，轻量 tag 和 annotated tag 均得到实际
commit，而不是依赖事件 SHA 的 tag-object 语义。发布前再通过 GitHub API 递归解析当前 tag，
只有它仍指向同一 commit 时才允许创建 Release。

## 3. Workflow 拓扑

```text
push v* tag
    |
    v
build (windows-latest)
    |  test -> ldflags build -> package -> SHA256SUMS
    +--------------------+
    v                    v
release               smoke
softprops release     gh release download -> unzip -> version/doctor NDJSON
```

`build` 只产生工作区资产；`release` 拥有最小的 `contents: write` 权限并上传 zip 与
`SHA256SUMS.txt`；`smoke` 依赖发布 job，重新下载已发布 zip 后验证真实产物。构建脚本不接触
token。强制移动 tag、无效 tag 和已存在 Release 直接失败，避免同一发布名被静默覆盖。

## 4. 资产布局与校验

zip 根目录固定包含 `auto-mas-runtime.exe`、仓库 `LICENSE` 和简要 `README.md`。校验文件使用
标准 SHA-256 两空格格式，至少覆盖 zip 资产本身，并作为独立 Release 资产发布。产物不包含
签名或私钥材料。

## 5. 验收边界

- T7.1：默认占位、ldflags 注入和 hello/version 共源均有确定性测试。
- T7.2：tag workflow 的触发、Windows 构建、资产命名、校验和、预发布判断和未签名发布可被
  静态检查并在本地复演打包命令。
- T7.3：冒烟 job 必须下载刚发布的 zip，解压后检查两个命令的退出码、首 `hello`、末 `result`
  和 NDJSON 行合法性。
- T7.4：本阶段明确跳过 CNB 同步，不创建需要未配置 secret 的外发 job。
