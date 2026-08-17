package uv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const maxUVLockFileBytes = 16 << 20

// DependenciesRequest 描述一次主项目依赖操作的固定工作区和 Python 身份。
type DependenciesRequest struct {
	ProjectDir    string
	ProjectEnvDir string
	PythonVersion string
	OperationID   string
	Branch        string
	Commit        string
	MirrorPolicy  mirror.Policy
	Line          LineFunc
}

// DependenciesResult 保存锁文件检查或同步后的稳定结果。
type DependenciesResult struct {
	LockfileChecked bool
	Synchronized    bool
	Rebuilt         bool
}

// DependenciesService 负责锁文件契约、项目模式同步和 managed venv 重建。
type DependenciesService struct {
	layout  *config.Layout
	runner  Runner
	remover TreeRemover
	network *networkExecutor
}

// NewDependenciesService 创建主项目依赖服务。
func NewDependenciesService(
	layout *config.Layout,
	runner Runner,
	remover TreeRemover,
) (*DependenciesService, error) {
	if layout == nil || runner == nil || remover == nil {
		return nil, errors.New("dependencies service dependencies are incomplete")
	}
	network, err := newDefaultNetworkExecutor()
	if err != nil {
		return nil, err
	}
	return &DependenciesService{layout: layout, runner: runner, remover: remover, network: network}, nil
}

// Check 只读检查 uv.lock 与现有主项目环境是否保持同步。
func (s *DependenciesService) Check(
	ctx context.Context,
	request DependenciesRequest,
) (DependenciesResult, error) {
	if err := s.validateRequest(ctx, &request); err != nil {
		return DependenciesResult{}, err
	}
	if err := s.checkLockfile(ctx, request); err != nil {
		return DependenciesResult{}, err
	}
	result, err := s.runner.Run(ctx, []string{
		"sync",
		"--project",
		request.ProjectDir,
		"--python",
		request.PythonVersion,
		"--check",
		"--locked",
		"--no-default-groups",
		"--no-install-workspace",
	}, withOfflineUV(s.runOptions(request, protocol.StageDependenciesCheck)))
	if err != nil || result.ExitCode != 0 {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return DependenciesResult{}, err
		}
		return DependenciesResult{}, dependencyCheckError(result, err)
	}
	return DependenciesResult{LockfileChecked: true, Synchronized: true}, nil
}

// Sync 在只读锁文件检查通过后执行固定的锁定依赖同步。
func (s *DependenciesService) Sync(
	ctx context.Context,
	request DependenciesRequest,
) (DependenciesResult, error) {
	if err := s.validateRequest(ctx, &request); err != nil {
		return DependenciesResult{}, err
	}
	if err := s.checkLockfile(ctx, request); err != nil {
		return DependenciesResult{}, err
	}
	lockDigest, err := lockfileDigest(ctx, s.lockfilePath(request.ProjectDir))
	if err != nil {
		return DependenciesResult{}, err
	}
	target, err := mirror.NewTarget(mirror.TargetSpec{LockDigest: lockDigest})
	if err != nil {
		return DependenciesResult{}, fmt.Errorf("build package index mirror target: %w", err)
	}
	result, err := s.network.run(ctx, s.runner, request.MirrorPolicy, mirror.KindPackageIndex, target, []string{
		"sync",
		"--project",
		request.ProjectDir,
		"--python",
		request.PythonVersion,
		"--locked",
		"--no-default-groups",
		"--no-install-workspace",
	}, s.runOptions(request, protocol.StageDependenciesSync))
	if err != nil || result.ExitCode != 0 {
		if isNetworkPolicyError(err) {
			return DependenciesResult{}, err
		}
		return DependenciesResult{}, dependencySyncError(result, err)
	}
	return DependenciesResult{LockfileChecked: true, Synchronized: true}, nil
}

func (s *DependenciesService) checkLockfile(ctx context.Context, request DependenciesRequest) error {
	if err := requireRegularLockfile(s.lockfilePath(request.ProjectDir)); err != nil {
		return err
	}
	args := []string{
		"lock",
		"--project",
		request.ProjectDir,
		"--check",
	}
	options := s.runOptions(request, protocol.StageDependenciesCheck)
	plan, err := s.network.plan(request.MirrorPolicy, mirror.KindPackageIndex)
	if err != nil {
		return err
	}
	if plan.Offline() {
		options = withOfflineUV(options)
	} else {
		sources := plan.Sources()
		if len(sources) == 0 {
			return errors.New("package index mirror plan is empty")
		}
		args = append(args, "--default-index", sources[0].BaseURL())
	}
	result, err := s.runner.Run(ctx, args, options)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if result.ExitCode > 0 {
			return newError(
				protocol.CodeLockfileOutdated,
				protocol.StageDependenciesCheck,
				"项目锁文件已过期",
				map[string]any{"exitCode": result.ExitCode},
				err,
			)
		}
		return err
	}
	if result.ExitCode != 0 {
		return newError(
			protocol.CodeLockfileOutdated,
			protocol.StageDependenciesCheck,
			"项目锁文件已过期",
			map[string]any{"exitCode": result.ExitCode},
			nil,
		)
	}
	return nil
}

func lockfileDigest(ctx context.Context, path string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", newError(
			protocol.CodeLockfileMissing,
			protocol.StageDependenciesCheck,
			"项目锁文件不可读取",
			map[string]any{},
			err,
		)
	}
	if len(contents) > maxUVLockFileBytes {
		return "", newError(
			protocol.CodeLockfileMissing,
			protocol.StageDependenciesCheck,
			"项目锁文件过大",
			map[string]any{"maxBytes": maxUVLockFileBytes},
			errors.New("uv.lock is too large"),
		)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:]), nil
}

// Rebuild 通过 T2.5 删除能力重建 managed venv，不触碰源码、锁文件和用户数据。
func (s *DependenciesService) Rebuild(
	ctx context.Context,
	request DependenciesRequest,
) (DependenciesResult, error) {
	if err := s.validateRequest(ctx, &request); err != nil {
		return DependenciesResult{}, err
	}
	result, err := s.remover.RemoveTree(ctx, filesystem.DeleteRequest{
		Kind:        filesystem.DeleteManagedVenv,
		Target:      request.ProjectEnvDir,
		OperationID: request.OperationID,
		Reason:      "rebuild managed environment",
	})
	if err != nil {
		return DependenciesResult{}, newError(
			protocol.CodeEnvironmentRebuildFailed,
			protocol.StageDependenciesRebuild,
			"主项目环境重建失败",
			map[string]any{"removed": result.Removed, "partial": result.Partial},
			err,
		)
	}
	return DependenciesResult{Rebuilt: result.Removed}, nil
}

func (s *DependenciesService) validateRequest(
	ctx context.Context,
	request *DependenciesRequest,
) error {
	if ctx == nil || s == nil || s.layout == nil || s.runner == nil || s.remover == nil || request == nil {
		return errors.New("dependencies request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.ProjectDir == "" {
		request.ProjectDir = s.layout.RepoDir()
	}
	if request.ProjectEnvDir == "" {
		request.ProjectEnvDir = s.layout.VenvDir()
	}
	if request.PythonVersion == "" {
		return newError(
			protocol.CodePythonVersionMismatch,
			protocol.StageDependenciesCheck,
			"缺少受管 Python 版本",
			map[string]any{},
			errors.New("python version is required for dependency operation"),
		)
	}
	for name, value := range map[string]string{
		"project directory":   request.ProjectDir,
		"project environment": request.ProjectEnvDir,
		"operation id":        request.OperationID,
		"python version":      request.PythonVersion,
	} {
		if value == "" || containsNUL(value) {
			return fmt.Errorf("%s is invalid", name)
		}
	}
	return nil
}

func (s *DependenciesService) lockfilePath(projectDir string) string {
	if projectDir == s.layout.RepoDir() {
		return s.layout.UVLockFile()
	}
	return filepath.Join(projectDir, "uv.lock")
}

func (s *DependenciesService) runOptions(
	request DependenciesRequest,
	stage protocol.Stage,
) RunOptions {
	return RunOptions{
		Stage:            stage,
		ProjectDir:       request.ProjectDir,
		ProjectEnvDir:    request.ProjectEnvDir,
		PythonInstallDir: s.layout.PythonDir(),
		CacheDir:         s.layout.UVCacheDir(),
		PythonVersion:    request.PythonVersion,
		Branch:           request.Branch,
		Commit:           request.Commit,
		Line:             request.Line,
	}
}

func requireRegularLockfile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return newError(
			protocol.CodeLockfileMissing,
			protocol.StageDependenciesCheck,
			"缺少项目锁文件",
			map[string]any{},
			err,
		)
	}
	if err != nil {
		return newError(
			protocol.CodeLockfileMissing,
			protocol.StageDependenciesCheck,
			"项目锁文件不可读取",
			map[string]any{},
			err,
		)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return newError(
			protocol.CodeLockfileMissing,
			protocol.StageDependenciesCheck,
			"项目锁文件不是普通文件",
			map[string]any{},
			errors.New("uv.lock is not a regular file"),
		)
	}
	if info.Size() > maxUVLockFileBytes {
		return newError(
			protocol.CodeLockfileMissing,
			protocol.StageDependenciesCheck,
			"项目锁文件过大",
			map[string]any{"maxBytes": maxUVLockFileBytes},
			errors.New("uv.lock is too large"),
		)
	}
	return nil
}

func dependencySyncError(result UVResult, cause error) error {
	if cause == nil {
		cause = errors.New("uv sync exited with a non-zero status")
	}
	return newError(
		protocol.CodeDependencySyncFailed,
		protocol.StageDependenciesSync,
		"Python 依赖同步失败",
		map[string]any{"exitCode": result.ExitCode},
		cause,
	)
}

func dependencyCheckError(result UVResult, cause error) error {
	if cause == nil {
		cause = errors.New("uv sync check exited with a non-zero status")
	}
	return newError(
		protocol.CodeDependencySyncFailed,
		protocol.StageDependenciesCheck,
		"主项目依赖环境未同步",
		map[string]any{"exitCode": result.ExitCode},
		cause,
	)
}

func containsNUL(value string) bool {
	for _, character := range value {
		if character == '\x00' {
			return true
		}
	}
	return false
}
