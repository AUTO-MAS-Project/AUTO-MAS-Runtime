package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/cleanup"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// cleanupCommand 注册 cleanup 命令并委托给安全清理服务。
func cleanupCommand(deps *deps) *cobra.Command {
	return &cobra.Command{
		Use:   "cleanup",
		Short: "清理可丢弃缓存与临时内容",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps.exitCode = runOperation(
				deps.ctx,
				deps,
				commandPath(cmd),
				protocol.StageCleanup,
				func(ctx context.Context, emitter *protocol.Emitter) (sessionSuccess, error) {
					service, err := cleanup.New(
						deps.global.layout,
						stderrAuditor{err: deps.io.Err},
					)
					if err != nil {
						return sessionSuccess{}, err
					}
					report, err := service.Run(ctx, emitter)
					if err != nil {
						return sessionSuccess{}, err
					}
					return sessionSuccess{
						message: "清理完成",
						details: cleanup.ReportDetails(report),
					}, nil
				},
			)
			return nil
		},
	}
}
