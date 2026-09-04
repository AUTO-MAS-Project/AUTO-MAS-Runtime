package uv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/logging"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// stagingCleanupTimeout 是临时项目目录收口的有界预算。
//
// 清理走脱离业务取消的 context，因此需要自己的超时，否则取消后可能永远等下去。
const stagingCleanupTimeout = 15 * time.Second

// MirrorAttempt 描述依赖同步镜像轮换中的一次尝试。
type MirrorAttempt struct {
	SourceKind string
	Source     string
	SourceTry  int
	GlobalTry  int
	// Fallback 为真表示这次用的是 repo 原锁与 --locked，即 C10 的回退路径。
	Fallback bool
}

// MirrorAttemptFunc 在每次尝试开始前报告当前源；返回错误会中止整轮轮换。
type MirrorAttemptFunc func(ctx context.Context, attempt MirrorAttempt) error

// DependenciesOption 配置 DependenciesService 的可注入依赖。
type DependenciesOption func(*dependenciesOptions) error

type dependenciesOptions struct {
	catalog        *mirror.Catalog
	rotator        sourceRotator
	stagingRemover TreeRemover
}

// WithDependenciesCatalog 注入包索引源目录。
func WithDependenciesCatalog(catalog *mirror.Catalog) DependenciesOption {
	return func(options *dependenciesOptions) error {
		if options == nil || catalog == nil {
			return errors.New("dependencies mirror catalog is invalid")
		}
		options.catalog = catalog
		return nil
	}
}

// WithDependenciesRotator 注入镜像轮换器。
func WithDependenciesRotator(rotator sourceRotator) DependenciesOption {
	return func(options *dependenciesOptions) error {
		if options == nil || rotator == nil {
			return errors.New("dependencies mirror rotator is invalid")
		}
		options.rotator = rotator
		return nil
	}
}

// WithDependenciesStagingRemover 注入临时项目目录的受控删除能力。
func WithDependenciesStagingRemover(remover TreeRemover) DependenciesOption {
	return func(options *dependenciesOptions) error {
		if options == nil || remover == nil {
			return errors.New("dependencies staging remover is invalid")
		}
		options.stagingRemover = remover
		return nil
	}
}

// syncWithMirrors 按镜像轮换执行 uv sync。
//
// 每个镜像源在受管临时项目目录里用改写后的锁副本执行 --frozen 安装；plan 末位的
// 官方源改用 repo 原锁与 --locked，这就是 C10 的「全部镜像失败后回退原锁」。
// --mirror-only 时 BuildPlan 本就不放官方源进 plan，因此不需要额外的分支来禁止回退。
func (s *DependenciesService) syncWithMirrors(
	ctx context.Context,
	request DependenciesRequest,
) (DependenciesResult, error) {
	lock, err := readManagedRegularFile(s.lockfilePath(request.ProjectDir), maxUVLockFileBytes)
	if err != nil {
		return DependenciesResult{}, newError(
			protocol.CodeLockfileMissing,
			protocol.StageDependenciesSync,
			"项目锁文件不可读取",
			map[string]any{},
			err,
		)
	}
	projectFile, err := readManagedRegularFile(
		filepath.Join(request.ProjectDir, "pyproject.toml"),
		maxPyProjectFileBytes,
	)
	if err != nil {
		return DependenciesResult{}, newError(
			protocol.CodeDependencySyncFailed,
			protocol.StageDependenciesSync,
			"项目声明文件不可读取",
			map[string]any{},
			err,
		)
	}
	digest := sha256.Sum256(lock)
	target, err := mirror.NewTarget(mirror.TargetSpec{LockDigest: hex.EncodeToString(digest[:])})
	if err != nil {
		return DependenciesResult{}, fmt.Errorf("build package index mirror target: %w", err)
	}
	plan, err := s.buildPackageIndexPlan(request.MirrorPolicy)
	if err != nil {
		return DependenciesResult{}, err
	}

	state := mirrorSyncState{}
	rotationResult, rotationErr := s.rotator.Run(
		ctx,
		plan,
		target,
		func(attemptCtx context.Context, attempt mirror.Attempt) mirror.AttemptOutcome {
			return s.runMirrorAttempt(attemptCtx, request, attempt, string(lock), string(projectFile), &state)
		},
	)
	if rotationErr == nil {
		return DependenciesResult{
			LockfileChecked: true,
			Synchronized:    true,
			SourceKind:      mirror.KindPackageIndex.String(),
			Source:          rotationResult.Source.Key(),
			AttemptCount:    state.attempts,
			LockRewritten:   !rotationResult.Source.Official(),
		}, nil
	}
	return DependenciesResult{}, s.mapRotationFailure(ctx, plan, state, rotationErr)
}

// buildPackageIndexPlan 生成包索引源的尝试顺序。
//
// 显式 --mirror package-index=<key> 由 BuildPlan 排在最前（C10 的 2026-09-01 修订）。
// 用户显式指定却选不出源时必须失败关闭，不能静默换成别的源；只有 Policy 自身结构
// 不合法（例如零值 Policy）才退回目录默认顺序，与 internal/uv 其他网络路径一致。
func (s *DependenciesService) buildPackageIndexPlan(policy mirror.Policy) (mirror.Plan, error) {
	plan, err := mirror.BuildPlan(s.catalog, policy, mirror.KindPackageIndex)
	if err == nil {
		return plan, nil
	}
	if errors.Is(err, mirror.ErrPolicyRejected) {
		return mirror.Plan{}, newError(
			protocol.CodeInvalidArgument,
			protocol.StageDependenciesSync,
			"镜像源选择无效",
			map[string]any{"sourceKind": mirror.KindPackageIndex.String()},
			err,
		)
	}
	defaultPolicy, defaultErr := mirror.NewPolicy(mirror.PolicySpec{Preferred: map[mirror.Kind]string{}})
	if defaultErr != nil {
		return mirror.Plan{}, fmt.Errorf("build default package index policy: %w", defaultErr)
	}
	plan, defaultErr = mirror.BuildPlan(s.catalog, defaultPolicy, mirror.KindPackageIndex)
	if defaultErr != nil {
		return mirror.Plan{}, fmt.Errorf("build package index mirror plan: %w", errors.Join(err, defaultErr))
	}
	return plan, nil
}

// mirrorSyncState 累积轮换过程中的可变事实，只在 Rotator 的串行回调里被写。
type mirrorSyncState struct {
	attempts   int
	lastResult UVResult
	lastErr    error
	notifyErr  error
}

func (s *DependenciesService) runMirrorAttempt(
	ctx context.Context,
	request DependenciesRequest,
	attempt mirror.Attempt,
	lock string,
	projectFile string,
	state *mirrorSyncState,
) mirror.AttemptOutcome {
	fallback := attempt.Source.Official()
	state.attempts++
	if err := notifyMirrorAttempt(ctx, request.Attempt, MirrorAttempt{
		SourceKind: attempt.Source.Kind().String(),
		Source:     attempt.Source.Key(),
		SourceTry:  attempt.SourceTry,
		GlobalTry:  attempt.GlobalTry,
		Fallback:   fallback,
	}); err != nil {
		// 报告失败意味着协议输出已经不可用，继续换源没有意义：
		// TargetFailure 是 Rotator 唯一会立刻结束整轮的失败结局。
		state.notifyErr = err
		return mirror.AttemptOutcome{
			Kind:        mirror.OutcomeTargetFailure,
			FailureKind: mirror.FailureKind("attempt_report"),
			Err:         err,
		}
	}
	if fallback {
		result, runErr := s.runner.Run(
			ctx,
			lockedSyncArguments(request.ProjectDir, request.PythonVersion),
			s.runOptions(request, protocol.StageDependenciesSync),
		)
		return s.finishAttempt(state, result, runErr)
	}
	rewrite, ok := attempt.Source.PackageIndexRewrite()
	if !ok {
		err := errors.New("package index source has no rewrite prefixes")
		state.lastErr = err
		return mirror.AttemptOutcome{
			Kind:        mirror.OutcomeSwitchSource,
			FailureKind: mirror.FailureKind("missing_rewrite"),
			Err:         err,
		}
	}
	result, runErr := s.runStagedSync(ctx, request, rewrite, lock, projectFile)
	return s.finishAttempt(state, result, runErr)
}

func (s *DependenciesService) finishAttempt(
	state *mirrorSyncState,
	result UVResult,
	err error,
) mirror.AttemptOutcome {
	state.lastResult = result
	if err == nil && result.ExitCode == 0 {
		state.lastErr = nil
		return mirror.AttemptOutcome{Kind: mirror.OutcomeSucceeded}
	}
	state.lastErr = nonNilRunError(err)
	// 每个源只尝试一次：uv sync 是重操作，uv 自身对单个 artifact 已有重试，
	// 同源重试要重建临时目录并重跑整条安装流程，断网时只会把等待时间翻倍。
	return mirror.AttemptOutcome{
		Kind:        mirror.OutcomeSwitchSource,
		FailureKind: mirror.FailureKind("uv_exec"),
		Err:         state.lastErr,
	}
}

// runStagedSync 在受管临时项目目录里用改写后的锁执行一次 --frozen 安装。
//
// 目录在返回前一定被删除：成功、失败与取消都走同一个 defer，取消时使用脱离
// 业务 context 的收口预算，因此不会把临时目录留在盘上。
func (s *DependenciesService) runStagedSync(
	ctx context.Context,
	request DependenciesRequest,
	rewrite mirror.PackageIndexRewrite,
	lock string,
	projectFile string,
) (result UVResult, returnErr error) {
	stagingDir, err := s.layout.DependencySyncDir(request.OperationID)
	if err != nil {
		return UVResult{}, err
	}
	// 先做一次幂等清理：上一个源用完的目录已经删了，但进程崩溃可能留下残留，
	// 而 PrepareManagedDirectory 要求目标不存在。
	if err := s.removeStagingProject(ctx, request.OperationID, stagingDir); err != nil {
		return UVResult{}, err
	}
	defer func() {
		cleanupCtx, cancel := stagingCleanupContext(ctx)
		defer cancel()
		if cleanupErr := s.removeStagingProject(cleanupCtx, request.OperationID, stagingDir); cleanupErr != nil {
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()
	rewritten := rewriteLockfile(lock, rewrite)
	if err := writeStagingProject(ctx, s.layout, stagingDir, projectFile, rewritten.Lock); err != nil {
		return UVResult{}, err
	}
	return s.runner.Run(
		ctx,
		mirrorSyncArguments(stagingDir, request.PythonVersion),
		s.runOptions(request, protocol.StageDependenciesSync),
	)
}

func (s *DependenciesService) removeStagingProject(
	ctx context.Context,
	operationID string,
	stagingDir string,
) error {
	if s.stagingRemover == nil {
		return errors.New("dependencies staging remover is unavailable")
	}
	result, err := s.stagingRemover.RemoveTree(ctx, filesystem.DeleteRequest{
		Kind:        filesystem.DeleteDependencySync,
		Target:      stagingDir,
		OperationID: operationID,
		Reason:      "remove dependency sync staging project",
	})
	if err != nil {
		return fmt.Errorf("remove dependency sync staging project: %w", err)
	}
	if result.Partial {
		return errors.New("dependency sync staging project removal was partial")
	}
	return nil
}

func (s *DependenciesService) mapRotationFailure(
	ctx context.Context,
	plan mirror.Plan,
	state mirrorSyncState,
	rotationErr error,
) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(rotationErr, context.Canceled) || errors.Is(rotationErr, context.DeadlineExceeded) {
		return rotationErr
	}
	if state.notifyErr != nil {
		return state.notifyErr
	}
	details := map[string]any{
		"sourceKind":   mirror.KindPackageIndex.String(),
		"attemptCount": state.attempts,
		"exitCode":     state.lastResult.ExitCode,
	}
	var rotationError *mirror.RotationError
	if errors.As(rotationErr, &rotationError) && rotationError.Code() == protocol.CodeMirrorExhausted {
		if planHasOfficialSource(plan) {
			// 回退那次也失败了，语义与今天的单次 --locked 失败完全一致。
			return newError(
				protocol.CodeDependencySyncFailed,
				protocol.StageDependenciesSync,
				"Python 依赖同步失败",
				details,
				errors.Join(state.lastErr, rotationErr),
			)
		}
		return newError(
			protocol.CodeMirrorExhausted,
			protocol.StageDependenciesSync,
			"所有镜像源均不可用",
			details,
			rotationErr,
		)
	}
	return rotationErr
}

func planHasOfficialSource(plan mirror.Plan) bool {
	for _, source := range plan.Sources() {
		if source.Official() {
			return true
		}
	}
	return false
}

func notifyMirrorAttempt(
	ctx context.Context,
	notify MirrorAttemptFunc,
	attempt MirrorAttempt,
) error {
	if notify == nil {
		return nil
	}
	return notify(ctx, attempt)
}

func mirrorSyncArguments(projectDir, pythonVersion string) []string {
	return []string{
		"sync",
		"--project",
		projectDir,
		"--python",
		pythonVersion,
		"--frozen",
		"--no-default-groups",
		"--no-install-workspace",
	}
}

func lockedSyncArguments(projectDir, pythonVersion string) []string {
	return []string{
		"sync",
		"--project",
		projectDir,
		"--python",
		pythonVersion,
		"--locked",
		"--no-default-groups",
		"--no-install-workspace",
	}
}

func stagingCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), stagingCleanupTimeout)
}

// writeStagingProject 在受管临时目录里写入 pyproject.toml 与改写后的锁副本。
//
// --no-install-workspace 排除了根项目本身，因此临时项目只需要这两个文件，
// 不需要 README、LICENSE 或任何源码。
func writeStagingProject(
	ctx context.Context,
	layout *config.Layout,
	stagingDir string,
	projectFile string,
	lock string,
) (returnErr error) {
	if err := ensureManagedDirectory(ctx, layout, filepath.Dir(stagingDir)); err != nil {
		return err
	}
	lease, err := filesystem.PrepareManagedDirectory(ctx, layout, stagingDir)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()
	if err := writeStagingFile(stagingDir, "pyproject.toml", projectFile); err != nil {
		return err
	}
	return writeStagingFile(stagingDir, "uv.lock", lock)
}

func writeStagingFile(stagingDir, name, contents string) (returnErr error) {
	info, err := os.Lstat(stagingDir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("dependency sync staging project is not a regular directory")
	}
	file, err := os.OpenFile(
		filepath.Join(stagingDir, name),
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		0o600,
	)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, closeErr)
		}
	}()
	_, err = io.WriteString(file, contents)
	return err
}

// filesystemDependencyRemover 用受控删除移除依赖同步的临时项目目录。
type filesystemDependencyRemover struct{ layout *config.Layout }

func (r filesystemDependencyRemover) RemoveTree(
	ctx context.Context,
	request filesystem.DeleteRequest,
) (filesystem.DeleteResult, error) {
	if r.layout == nil {
		return filesystem.DeleteResult{}, errors.New("dependency staging remover layout is invalid")
	}
	logger, err := logging.New(ctx, r.layout, io.Discard, "dependencies-sync", request.OperationID)
	if err != nil {
		return filesystem.DeleteResult{}, err
	}
	operator, err := filesystem.New(ctx, r.layout, deletionLogger{logger: logger})
	if err != nil {
		return filesystem.DeleteResult{}, errors.Join(err, logger.Close())
	}
	result, removeErr := operator.RemoveTree(ctx, request)
	return result, errors.Join(removeErr, logger.Close())
}
