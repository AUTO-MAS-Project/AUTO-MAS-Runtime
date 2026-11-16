//go:build linux || darwin

package uv

import (
	"os/exec"
	"syscall"
)

// configureUVCommand 保留非 Windows 编译边界，平台支持仍由 process 包判定。
func configureUVCommand(command *exec.Cmd) { command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
