//go:build !windows

package uv

import "os/exec"

// configureUVCommand 保留非 Windows 编译边界，平台支持仍由 process 包判定。
func configureUVCommand(_ *exec.Cmd) {}
