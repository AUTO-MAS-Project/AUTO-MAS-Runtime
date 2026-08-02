// Package doctor 提供只读运行环境诊断服务。
//
// 所有检查只读取受管根目录；doctor 不获取 mutation/backend 锁、不创建、
// 不修改也不删除任何持久化状态。网络、uv 版本与磁盘探测通过 Probes 注入，
// 单元测试不触碰真实公网或用户机器工具。
package doctor
