package cli

import (
	"context"
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/backend"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const backendModeManaged = "managed"

func backendSuperviseCommand(deps *deps) *cobra.Command {
	var mode string
	command := &cobra.Command{
		Use:   "supervise",
		Short: "启动并监督后端进程",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			deps.exitCode = runOperationWithCapabilities(
				deps.ctx,
				deps,
				commandPath(cmd),
				protocol.StageBackendSpawn,
				[]string{string(protocol.CapabilityStateV1), string(protocol.CapabilityLogStream)},
				func(ctx context.Context, emitter *protocol.Emitter) (sessionSuccess, error) {
					if mode == "" {
						return sessionSuccess{}, &commandError{
							code:    protocol.CodeInvalidArgument,
							stage:   protocol.StageBackendSpawn,
							message: "必须显式指定后端运行模式",
							details: map[string]any{"field": "mode"},
							cause:   errors.New("backend mode is required"),
						}
					}
					if mode != backendModeManaged {
						return sessionSuccess{}, &commandError{
							code:    protocol.CodeUnsupportedMode,
							stage:   protocol.StageBackendSpawn,
							message: "当前后端运行模式尚不受支持",
							details: map[string]any{"mode": mode},
							cause:   errors.New("backend mode is unsupported"),
						}
					}
					service, err := deps.options.backendFactory(
						ctx,
						deps.global.layout,
						deps.io.Err,
						deps.options.clock,
					)
					if err != nil {
						return sessionSuccess{}, err
					}
					if service == nil {
						return sessionSuccess{}, &commandError{
							code:    protocol.CodeInternalError,
							stage:   protocol.StageBackendSpawn,
							message: "后端监督器初始化失败",
							details: map[string]any{},
							cause:   errors.New("backend service is nil"),
						}
					}
					pid := os.Getpid()
					if pid <= 0 {
						return sessionSuccess{}, &commandError{
							code:    protocol.CodeInternalError,
							stage:   protocol.StageBackendSpawn,
							message: "Runtime 进程身份不可用",
							details: map[string]any{},
							cause:   errors.New("runtime pid is invalid"),
						}
					}
					if err := service.Supervise(ctx, backend.Request{
						OperationID: emitter.OperationID(),
						RuntimePID:  uint32(pid),
						Emitter:     emitter,
					}); err != nil {
						return sessionSuccess{}, err
					}
					return sessionSuccess{
						message: "后端监督已停止",
						details: map[string]any{},
						status:  string(protocol.StateStopped),
					}, nil
				},
			)
			return nil
		},
	}
	command.Flags().StringVar(&mode, "mode", "", "后端运行模式：managed 或 development")
	return command
}
