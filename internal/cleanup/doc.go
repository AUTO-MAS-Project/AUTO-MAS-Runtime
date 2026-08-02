// Package cleanup 提供受管可丢弃缓存与临时内容的清理服务。
//
// cleanup 获取 mutation Mutex 后，只通过 filesystem.Operator 的受控删除
// 能力删除明确授权目标；用户数据、插件目录与诊断日志来自 config 保护名单，
// 一律不触碰。repo.update-* 只有在事务状态、目录身份与 PID 事实全部匹配
// 时才允许删除，身份不明时失败关闭。
package cleanup
