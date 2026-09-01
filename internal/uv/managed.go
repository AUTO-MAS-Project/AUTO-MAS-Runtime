package uv

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// SupervisionIdentity 保存 managed 模式必须注入后端的版本与提交身份；development 传 nil。
type SupervisionIdentity struct {
	Version string
	Commit  string
}

// SupervisionInfrastructure 描述按增补 1 C11 下发给后端的受管基础设施事实。
//
// 四项在 managed 与 development 两种模式下都会被注入。两个目录留空时回退到本次
// uv 调用实际解析出的受管目录，保证下发值与 UV_CACHE_DIR / UV_PYTHON_INSTALL_DIR
// 永远同源——C11 要的是「共用一份缓存与一份解释器」，两者一旦分叉契约即失效。
// 两个源列表按 Runtime 解析后的尝试顺序排列，空切片对应 --offline 的空串注入。
type SupervisionInfrastructure struct {
	UVCacheDir          string
	PythonInstallDir    string
	PackageIndexSources []string
	PythonSources       []string
}

// ManagedOptions 把通用 uv 选项与长驻监督身份策略分开，避免调用方直接拼受控环境键。
type ManagedOptions struct {
	RunOptions
	Identity       *SupervisionIdentity
	Infrastructure SupervisionInfrastructure
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
	infrastructure, err := resolveSupervisionInfrastructure(resolved, options.Infrastructure)
	if err != nil {
		return nil, newError(
			protocol.CodeUVExecFailed,
			options.Stage,
			"uv 执行失败",
			map[string]any{},
			err,
		)
	}
	for key, value := range infrastructure {
		supervision[key] = value
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
		Dir:        resolved.WorkingDir,
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

// resolveSupervisionInfrastructure 把 C11 的四个受控键解析成最终注入值。
//
// 目录必须是绝对路径并被规范化；源列表逐项校验后用 `;` 连接，空列表得到空串
// 但键仍然存在（契约表两列都是「必填」，空串是合法取值、缺键不是）。
func resolveSupervisionInfrastructure(
	resolved resolvedRunOptions,
	requested SupervisionInfrastructure,
) (map[string]string, error) {
	cacheDir := requested.UVCacheDir
	if cacheDir == "" {
		cacheDir = resolved.CacheDir
	}
	pythonInstallDir := requested.PythonInstallDir
	if pythonInstallDir == "" {
		pythonInstallDir = resolved.PythonInstallDir
	}
	cleanCacheDir, err := canonicalSupervisionDirectory(cacheDir)
	if err != nil {
		return nil, fmt.Errorf("resolve supervised uv cache directory: %w", err)
	}
	cleanPythonInstallDir, err := canonicalSupervisionDirectory(pythonInstallDir)
	if err != nil {
		return nil, fmt.Errorf("resolve supervised python install directory: %w", err)
	}
	packageIndex, err := joinSupervisionMirrorSources(requested.PackageIndexSources)
	if err != nil {
		return nil, fmt.Errorf("resolve supervised package index sources: %w", err)
	}
	python, err := joinSupervisionMirrorSources(requested.PythonSources)
	if err != nil {
		return nil, fmt.Errorf("resolve supervised python sources: %w", err)
	}
	return map[string]string{
		autoMASUVCacheDir:         cleanCacheDir,
		autoMASUVPythonInstallDir: cleanPythonInstallDir,
		autoMASMirrorPackageIndex: packageIndex,
		autoMASMirrorPython:       python,
	}, nil
}

func canonicalSupervisionDirectory(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, '\x00') {
		return "", errors.New("supervised directory is invalid")
	}
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return "", errors.New("supervised directory must be absolute")
	}
	return cleaned, nil
}

func joinSupervisionMirrorSources(sources []string) (string, error) {
	for _, source := range sources {
		if source == "" || strings.ContainsRune(source, '\x00') {
			return "", errors.New("supervised mirror source is empty")
		}
		if strings.Contains(source, mirrorSourceSeparator) {
			return "", errors.New("supervised mirror source must not contain the list separator")
		}
	}
	return strings.Join(sources, mirrorSourceSeparator), nil
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
