package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/doctor"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// doctorCommand 注册 doctor 命令并委托给只读诊断服务。
func doctorCommand(deps *deps) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "只读检查本机运行环境",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps.exitCode = runOperation(
				deps.ctx,
				deps,
				commandPath(cmd),
				protocol.StageDoctor,
				func(ctx context.Context, emitter *protocol.Emitter) (sessionSuccess, error) {
					service, err := deps.options.doctorFactory(
						deps.global.layout,
						doctor.ProductionProbes(),
					)
					if err != nil {
						return sessionSuccess{}, err
					}
					report, err := service.Run(ctx, emitter)
					if err != nil {
						return sessionSuccess{}, err
					}
					return sessionSuccess{
						message: "诊断完成",
						details: map[string]any{
							"checks":  checksToWire(report.Checks),
							"summary": summaryToWire(report.Summary),
						},
					}, nil
				},
			)
			return nil
		},
	}
}

// checksToWire 把检查项转换为 result.details 中的稳定对象数组。
func checksToWire(checks []doctor.Check) []map[string]any {
	wire := make([]map[string]any, 0, len(checks))
	for _, check := range checks {
		details := check.Details
		if details == nil {
			details = map[string]any{}
		}
		wire = append(wire, map[string]any{
			"id":      check.ID,
			"name":    check.Name,
			"status":  check.Status.String(),
			"message": check.Message,
			"details": details,
		})
	}
	return wire
}

// summaryToWire 把检查计数转换为 result.details 中的稳定对象。
func summaryToWire(summary doctor.Summary) map[string]any {
	return map[string]any{
		"total":   summary.Total,
		"ok":      summary.OK,
		"missing": summary.Missing,
		"error":   summary.Error,
	}
}
