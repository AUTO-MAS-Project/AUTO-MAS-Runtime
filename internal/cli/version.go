package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// versionCommand 注册 version 命令并委托给版本信息会话。
func versionCommand(deps *deps) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "显示 Runtime 与协议版本",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			deps.exitCode = runOperation(
				deps.ctx,
				deps,
				commandPath(cmd),
				protocol.StageRuntimeHandshake,
				func(ctx context.Context, emitter *protocol.Emitter) (sessionSuccess, error) {
					return runVersion(ctx, deps.options.versionSource, emitter)
				},
			)
			return nil
		},
	}
}

// runVersion 执行 version 命令的会话主体：查询版本信息并发射 progress 事件。
func runVersion(
	ctx context.Context,
	source versionSourceFunc,
	emitter *protocol.Emitter,
) (sessionSuccess, error) {
	info, err := source(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return sessionSuccess{}, err
		}
		var operationErr operationError
		if errors.As(err, &operationErr) {
			return sessionSuccess{}, err
		}
		return sessionSuccess{}, &commandError{
			code:    protocol.CodeOutputWriteFailed,
			stage:   protocol.StageRuntimeHandshake,
			message: "无法获取版本信息",
			details: map[string]any{},
			cause:   err,
		}
	}
	summary := fmt.Sprintf("Runtime %s，协议 %d", info.Version, info.Protocol)
	if err := emitter.EmitProgress(protocol.ProgressEvent{
		Stage:   protocol.StageRuntimeHandshake,
		Status:  protocol.ProgressSucceeded,
		Message: summary,
	}); err != nil {
		return sessionSuccess{}, err
	}
	return sessionSuccess{
		message: "版本信息查询完成",
		details: map[string]any{
			"runtimeVersion":  info.Version,
			"protocolVersion": info.Protocol,
			"commit":          info.Commit,
			"buildDate":       info.BuildDate,
			"goVersion":       info.GoVersion,
		},
	}, nil
}
