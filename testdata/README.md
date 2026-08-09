# 测试夹具

组件测试在运行时使用 go-git 动态创建裸仓库、提交图和智能 HTTPS upload-pack 服务，测试
CA 仅注入 go-git 的请求选项并始终保持 TLS 校验。服务通过 channel barrier 注入 discovery、
pack 和响应中断，同时记录 Depth、分支移动和请求次数；测试结束会断言受管根内没有
`repo.update-*` / `repo.previous-*` 残留。

本目录不提交 Git 二进制、真实仓库快照或证书私钥。只有需要跨测试复用且无法小型化的静态
文本夹具才应放在这里，并在对应任务文档中记录来源、用途和清理边界。

## 可执行夹具

- `fakeuv`：按 JSON 配置回放 uv 参数、环境、stdout/stderr、退出码和有限副作用；
- `fakebackend`：通过 `FAKE_BACKEND_CONFIG=<json 文件>` 配置监听延迟、health 序列、
  原始/畸形 health body、HTTP 状态、protocol/version/commit、崩溃退出、close 接受或拒绝、
  stdout/stderr 事件和孙进程。配置采用严格 JSON 解码并拒绝未知字段和越界值。默认监听协议
  固定端口 `127.0.0.1:36163`；夹具自测显式使用 `127.0.0.1:0`，不争抢生产契约端口。
  `readyFile` 写入实际 base URL，`grandchildPidFile` 只用于测试断言和收口；孙进程默认随父进程
  退出，`leaveGrandchildOnCrash` 仅用于 Job Object 异常回收测试，并会让孙进程继续持有继承的
  stdout/stderr 管道。
