# 测试夹具

组件测试在运行时使用 go-git 动态创建裸仓库、提交图和智能 HTTPS upload-pack 服务，测试
CA 仅注入 go-git 的请求选项并始终保持 TLS 校验。服务通过 channel barrier 注入 discovery、
pack 和响应中断，同时记录 Depth、分支移动和请求次数；测试结束会断言受管根内没有
`repo.update-*` / `repo.previous-*` 残留。

本目录不提交 Git 二进制、真实仓库快照或证书私钥。只有需要跨测试复用且无法小型化的静态
文本夹具才应放在这里，并在对应任务文档中记录来源、用途和清理边界。
