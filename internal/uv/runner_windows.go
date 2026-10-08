//go:build windows

package uv

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureUVCommand(command *exec.Cmd) {
	// 版本和 doctor 探针同样从桌面宿主启动，必须在创建进程时禁止分配控制台。
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}
