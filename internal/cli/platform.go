package cli

import "github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"

// platformCommandError 在原生安全机制通过前关闭未实现的命令，必须先于业务工厂运行。
func platformCommandError(goos, command string, stage protocol.Stage) error {
	if goos == "windows" || command == "version" {
		return nil
	}
	return &commandError{
		code:    protocol.CodeUnsupportedMode,
		stage:   stage,
		message: "当前平台的运行时能力尚未完成原生验收",
		details: map[string]any{"platform": goos, "reason": "platform_not_validated"},
	}
}
