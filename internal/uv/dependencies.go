package uv

import (
	"context"
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
	// Attempt 在镜像轮换的每次尝试开始前报告当前源，可为 nil。
	Attempt MirrorAttemptFunc
}

// DependenciesResult 保存锁文件检查或同步后的稳定结果。
type DependenciesResult struct {
	LockfileChecked bool
	Synchronized    bool
	Rebuilt         bool
	// SourceKind 恒为 package-index，同步执行过才有值。
	SourceKind string
	// Source 是实际完成同步的源 key；回退原锁时为官方源 key，离线时为空。
	Source string
	// AttemptCount 是实际执行 uv sync 的次数，含回退那次。
	AttemptCount int
	// LockRewritten 报告成功那次是否用了改写后的锁副本。
	LockRewritten bool
}

// DependenciesService 负责锁文件契约、项目模式同步和 managed venv 重建。
type DependenciesService struct {
	layout         *config.Layout
	runner         Runner
	remover        TreeRemover
	catalog        *mirror.Catalog
	rotator        sourceRotator
	stagingRemover TreeRemover
}

// NewDependenciesService 创建主项目依赖服务。
func NewDependenciesService(
	layout *config.Layout,
	runner Runner,
	remover TreeRemover,
	options ...DependenciesOption,
) (*DependenciesService, error) {
	if layout == nil || runner == nil || remover == nil {
		return nil, errors.New("dependencies service dependencies are incomplete")
	}
	configured := dependenciesOptions{stagingRemover: filesystemDependencyRemover{layout: layout}}
	for index, option := range options {
		if option == nil {
			return nil, fmt.Errorf("dependencies option at index %d is nil", index)
		}
		if err := option(&configured); err != nil {
			return nil, err
		}
	}
	if configured.catalog == nil {
		catalog, err := mirror.DefaultCatalog()
		if err != nil {
			return nil, fmt.Errorf("build dependencies mirror catalog: %w", err)
		}
		configured.catalog = catalog
	}
	if configured.rotator == nil {
		// 每个源只尝试一次，见 runStagedSync 的说明。
		rotator, err := mirror.NewRotator(mirror.WithMaxSourceAttempts(1))
		if err != nil {
			return nil, fmt.Errorf("build dependencies mirror rotator: %w", err)
		}
		configured.rotator = rotator
	}
	return &DependenciesService{
		layout:         layout,
		runner:         runner,
		remover:        remover,
		catalog:        configured.catalog,
		rotator:        configured.rotator,
		stagingRemover: configured.stagingRemover,
	}, nil
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

// Sync 在只读锁文件检查通过后执行锁定依赖同步。
//
// 在线时按 C10 走包索引镜像轮换：每个镜像用改写后的锁副本安装，全部失败后
// 回退到 repo 原锁。离线时完全不改写，沿用原锁并只注入 UV_OFFLINE=1。
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
	if request.MirrorPolicy.Offline() {
		return s.syncOffline(ctx, request)
	}
	return s.syncWithMirrors(ctx, request)
}

func (s *DependenciesService) syncOffline(
	ctx context.Context,
	request DependenciesRequest,
) (DependenciesResult, error) {
	result, err := s.runner.Run(
		ctx,
		lockedSyncArguments(request.ProjectDir, request.PythonVersion),
		withOfflineUV(s.runOptions(request, protocol.StageDependenciesSync)),
	)
	if err != nil || result.ExitCode != 0 {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return DependenciesResult{}, err
		}
		return DependenciesResult{}, newError(
			protocol.CodeNetworkUnavailable,
			protocol.StageDependenciesSync,
			"离线缓存不足，操作需要网络",
			map[string]any{
				"sourceKind":   mirror.KindPackageIndex.String(),
				"exitCode":     result.ExitCode,
				"attemptCount": 1,
			},
			nonNilRunError(err),
		)
	}
	return DependenciesResult{
		LockfileChecked: true,
		Synchronized:    true,
		SourceKind:      mirror.KindPackageIndex.String(),
		AttemptCount:    1,
	}, nil
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
	if request.MirrorPolicy.Offline() {
		options = withOfflineUV(options)
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
	if source, ok := request.MirrorPolicy.Preferred(mirror.KindPackageIndex); ok {
		return newError(
			protocol.CodeInvalidArgument,
			protocol.StageDependenciesCheck,
			"锁定依赖不支持覆盖包索引",
			map[string]any{"sourceKind": mirror.KindPackageIndex.String(), "source": source},
			errors.New("package index override conflicts with locked sources"),
		)
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
