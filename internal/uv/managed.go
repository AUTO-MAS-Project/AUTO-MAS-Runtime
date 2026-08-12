package uv

import (
	"context"
	"errors"
	"strings"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// SupervisionIdentity 保存 managed 模式必须注入后端的版本与提交身份；development 传 nil。
type SupervisionIdentity struct {
	Version string
	Commit  string
}

// ManagedOptions 把通用 uv 选项与长驻监督身份策略分开，避免调用方直接拼受控环境键。
type ManagedOptions struct {
	RunOptions
	Identity *SupervisionIdentity
}

// StartManaged 复用 UVRunner 的路径与环境策略启动长驻 uv，且不提供普通 exec 降级。
func (r *UVRunner) StartManaged(
	ctx context.Context,
	args []string,
	options ManagedOptions,
	sink process.StreamSink,
) (*process.ManagedProcess, error) {
	if ctx == nil {
		return nil, newError(
			protocol.CodeUVExecFailed,
			options.Stage,
			"uv 执行失败",
			map[string]any{},
			errors.New("uv runner context is nil"),
		)
	}
	if r == nil || r.Executable == "" || len(args) == 0 {
		return nil, newError(
			protocol.CodeUVExecFailed,
			options.Stage,
			"uv 执行失败",
			map[string]any{},
			errors.New("uv runner request is invalid"),
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, runnerCancellationError(options.Stage, map[string]any{}, err)
	}
	resolved := r.resolveOptions(options.RunOptions)
	supervision := map[string]string{
		autoMASUVExecutable: r.Executable,
		autoMASProtocol:     "1",
		autoMASSupervised:   "1",
	}
	if options.Identity != nil {
		if err := validateSupervisionIdentity(*options.Identity); err != nil {
			return nil, newError(
				protocol.CodeUVExecFailed,
				options.Stage,
				"uv 执行失败",
				map[string]any{},
				err,
			)
		}
		supervision[autoMASVersion] = options.Identity.Version
		supervision[autoMASCommit] = options.Identity.Commit
	}
	if err := validateRunnerPaths(resolved); err != nil {
		return nil, newError(
			protocol.CodeUVExecFailed,
			options.Stage,
			"uv 执行失败",
			map[string]any{},
			err,
		)
	}
	managed, err := process.StartManaged(ctx, process.StartSpec{
		Executable: r.Executable,
		Args:       append([]string(nil), args...),
		Dir:        resolved.ProjectDir,
		Env:        buildEnvironmentWithSupervision(resolved, supervision),
		Sink:       sink,
	})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil && errors.Is(err, contextErr) {
			return nil, runnerCancellationError(options.Stage, map[string]any{}, contextErr)
		}
		return nil, newError(
			protocol.CodeUVExecFailed,
			options.Stage,
			"uv 受管进程启动失败",
			startFailureDetails(resolved, err),
			err,
		)
	}
	return managed, nil
}

func validateSupervisionIdentity(identity SupervisionIdentity) error {
	if !validSupervisionVersion(identity.Version) {
		return errors.New("managed supervision version is invalid")
	}
	if len(identity.Commit) != 40 || identity.Commit != strings.ToLower(identity.Commit) {
		return errors.New("managed supervision commit must be 40 lowercase hexadecimal characters")
	}
	for _, character := range identity.Commit {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return errors.New("managed supervision commit must be 40 lowercase hexadecimal characters")
		}
	}
	return nil
}

func validSupervisionVersion(version string) bool {
	if len(version) < 2 || len(version) > 128 || version[0] != 'v' ||
		strings.Contains(version, "..") || strings.Contains(version, "@{") ||
		strings.HasSuffix(version, ".") || strings.HasSuffix(version, ".lock") {
		return false
	}
	for index := 1; index < len(version); index++ {
		character := version[index]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '.' || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}
