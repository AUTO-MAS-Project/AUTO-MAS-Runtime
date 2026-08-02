// Package version 提供 Runtime 版本与构建信息的注入点。
//
// Version、Commit 与 BuildDate 由构建期 -ldflags 写入，未注入时保持稳定占位；
// hello.runtimeVersion 与 version 命令共享同一份 Info 快照。
package version
