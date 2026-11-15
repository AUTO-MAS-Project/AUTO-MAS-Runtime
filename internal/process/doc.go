// Package process 负责 Runtime 创建进程及其普通后代的原生监督与回收。
//
// Windows 使用 Job Object；Linux/macOS 使用受 guardian 保护的 session。
// 游戏与模拟器只有显式请求平台原生脱离才在回收范围之外：Windows 使用
// CREATE_BREAKAWAY_FROM_JOB，POSIX 使用新 session。脱离者由创建方负责，
// Runtime 不按进程名称寻找或终止它们。
package process
