package uv

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

// UVOperations 是环境编排消费的 uv bootstrap/check 能力。
type UVOperations interface {
	Ensure(context.Context, string, mirror.Policy) (string, error)
	Check(context.Context) (bool, error)
}

type UVRepairOperations interface {
	Repair(context.Context, string, mirror.Policy) (string, error)
}

// PythonOperations 是环境编排消费的 Python prepare/check 能力。
type PythonOperations interface {
	Prepare(context.Context, PythonRequest) (PythonResult, error)
	Check(context.Context, PythonRequest) (PythonCheckResult, error)
}

// DependencyOperations 是环境编排消费的主项目依赖 sync/check 能力。
type DependencyOperations interface {
	Sync(context.Context, DependenciesRequest) (DependenciesResult, error)
	Check(context.Context, DependenciesRequest) (DependenciesResult, error)
	Rebuild(context.Context, DependenciesRequest) (DependenciesResult, error)
}

// EnvironmentRequest 描述一次 managed 环境 ensure/check。
type EnvironmentRequest struct {
	ProjectDir       string
	ProjectEnvDir    string
	PythonInstallDir string
	CacheDir         string
	PythonVersion    string
	OperationID      string
	Branch           string
	Commit           string
	BootstrapPolicy  mirror.Policy
	Line             LineFunc
}

// EnvironmentResult 保存环境操作的各阶段结果。
type EnvironmentResult struct {
	UVExecutable string
	UVReady      bool
	Python       PythonSpec
	Dependencies DependenciesResult
}

// EnvironmentService 编排 uv、Python 与主项目依赖，不启动后端。
type EnvironmentService struct {
	uv           UVOperations
	python       PythonOperations
	dependencies DependencyOperations
}

// NewEnvironmentService 创建环境编排服务。
func NewEnvironmentService(
	uv UVOperations,
	python PythonOperations,
	dependencies DependencyOperations,
) (*EnvironmentService, error) {
	if uv == nil || python == nil || dependencies == nil {
		return nil, errors.New("environment service dependencies are incomplete")
	}
	return &EnvironmentService{uv: uv, python: python, dependencies: dependencies}, nil
}

// Ensure 按 uv → Python → 锁定依赖顺序幂等准备 managed 环境。
func (s *EnvironmentService) Ensure(
	ctx context.Context,
	request EnvironmentRequest,
) (EnvironmentResult, error) {
	if ctx == nil || s == nil || s.uv == nil || s.python == nil || s.dependencies == nil {
		return EnvironmentResult{}, errors.New("environment ensure request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return EnvironmentResult{}, err
	}
	uvExecutable, err := s.EnsureUVWithLine(ctx, request.OperationID, request.BootstrapPolicy, request.Line)
	if err != nil {
		return EnvironmentResult{}, err
	}
	pythonResult, err := s.PreparePython(ctx, PythonRequest{
		ProjectDir:       request.ProjectDir,
		PythonInstallDir: request.PythonInstallDir,
		ProjectEnvDir:    request.ProjectEnvDir,
		CacheDir:         request.CacheDir,
		Branch:           request.Branch,
		Commit:           request.Commit,
		Line:             request.Line,
	})
	if err != nil {
		return EnvironmentResult{}, err
	}
	dependencyResult, err := s.SyncDependencies(ctx, DependenciesRequest{
		ProjectDir:    request.ProjectDir,
		ProjectEnvDir: request.ProjectEnvDir,
		PythonVersion: pythonResult.Spec.Version.String(),
		OperationID:   request.OperationID,
		Branch:        request.Branch,
		Commit:        request.Commit,
		Line:          request.Line,
	})
	if err != nil {
		return EnvironmentResult{}, err
	}
	return EnvironmentResult{
		UVExecutable: uvExecutable,
		UVReady:      true,
		Python:       pythonResult.Spec,
		Dependencies: dependencyResult,
	}, nil
}

// Check 只读取 uv、项目 Python 契约和锁定依赖事实，不安装或删除任何内容。
func (s *EnvironmentService) Check(
	ctx context.Context,
	request EnvironmentRequest,
) (EnvironmentResult, error) {
	if ctx == nil || s == nil || s.uv == nil || s.python == nil || s.dependencies == nil {
		return EnvironmentResult{}, errors.New("environment check request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return EnvironmentResult{}, err
	}
	uvReady, err := s.CheckUVWithLine(ctx, request.Line)
	if err != nil {
		return EnvironmentResult{}, err
	}
	if !uvReady {
		return EnvironmentResult{UVReady: false}, nil
	}
	pythonResult, err := s.CheckPython(ctx, PythonRequest{
		ProjectDir:       request.ProjectDir,
		PythonInstallDir: request.PythonInstallDir,
		ProjectEnvDir:    request.ProjectEnvDir,
		CacheDir:         request.CacheDir,
		Branch:           request.Branch,
		Commit:           request.Commit,
		Line:             request.Line,
	})
	if err != nil {
		return EnvironmentResult{}, err
	}
	dependencyResult, err := s.CheckDependencies(ctx, DependenciesRequest{
		ProjectDir:    request.ProjectDir,
		ProjectEnvDir: request.ProjectEnvDir,
		PythonVersion: pythonResult.Spec.Version.String(),
		OperationID:   request.OperationID,
		Branch:        request.Branch,
		Commit:        request.Commit,
		Line:          request.Line,
	})
	if err != nil {
		return EnvironmentResult{}, err
	}
	return EnvironmentResult{
		UVReady:      uvReady,
		Python:       pythonResult.Spec,
		Dependencies: dependencyResult,
	}, nil
}

// EnsureUV 只执行固定 uv bootstrap，供 bootstrap 顶层编排复用。
func (s *EnvironmentService) EnsureUV(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
) (string, error) {
	return s.ensureUV(ctx, operationID, policy, nil)
}

// EnsureUVWithLine 只执行固定 uv bootstrap，并转发版本检查输出。
func (s *EnvironmentService) EnsureUVWithLine(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
	line LineFunc,
) (string, error) {
	return s.ensureUV(ctx, operationID, policy, line)
}

func (s *EnvironmentService) ensureUV(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
	line LineFunc,
) (string, error) {
	if ctx == nil || s == nil || s.uv == nil {
		return "", errors.New("uv ensure request is invalid")
	}
	if withLine, ok := s.uv.(interface {
		EnsureWithLine(context.Context, string, mirror.Policy, LineFunc) (string, error)
	}); ok {
		return withLine.EnsureWithLine(ctx, operationID, policy, line)
	}
	return s.uv.Ensure(ctx, operationID, policy)
}

// CheckUV 只读取固定 uv 的可用性。
func (s *EnvironmentService) CheckUV(ctx context.Context) (bool, error) {
	return s.checkUV(ctx, nil)
}

// CheckUVWithLine 只读取固定 uv 的可用性，并转发版本检查输出。
func (s *EnvironmentService) CheckUVWithLine(ctx context.Context, line LineFunc) (bool, error) {
	return s.checkUV(ctx, line)
}

func (s *EnvironmentService) checkUV(ctx context.Context, line LineFunc) (bool, error) {
	if ctx == nil || s == nil || s.uv == nil {
		return false, errors.New("uv check request is invalid")
	}
	if withLine, ok := s.uv.(interface {
		CheckWithLine(context.Context, LineFunc) (bool, error)
	}); ok {
		return withLine.CheckWithLine(ctx, line)
	}
	return s.uv.Check(ctx)
}

// PreparePython 读取项目契约并准备精确受管 Python。
func (s *EnvironmentService) PreparePython(
	ctx context.Context,
	request PythonRequest,
) (PythonResult, error) {
	if ctx == nil || s == nil || s.python == nil {
		return PythonResult{}, errors.New("python preparation request is invalid")
	}
	return s.python.Prepare(ctx, request)
}

// CheckPython 只读取项目 Python 契约与受管安装事实。
func (s *EnvironmentService) CheckPython(
	ctx context.Context,
	request PythonRequest,
) (PythonCheckResult, error) {
	if ctx == nil || s == nil || s.python == nil {
		return PythonCheckResult{}, errors.New("python check request is invalid")
	}
	return s.python.Check(ctx, request)
}

// SyncDependencies 执行只读锁文件检查和 locked sync。
func (s *EnvironmentService) SyncDependencies(
	ctx context.Context,
	request DependenciesRequest,
) (DependenciesResult, error) {
	if ctx == nil || s == nil || s.dependencies == nil {
		return DependenciesResult{}, errors.New("dependency sync request is invalid")
	}
	return s.dependencies.Sync(ctx, request)
}

// CheckDependencies 只读取锁文件与 uv lock 检查结果。
func (s *EnvironmentService) CheckDependencies(
	ctx context.Context,
	request DependenciesRequest,
) (DependenciesResult, error) {
	if ctx == nil || s == nil || s.dependencies == nil {
		return DependenciesResult{}, errors.New("dependency check request is invalid")
	}
	return s.dependencies.Check(ctx, request)
}

// RebuildDependencies 只删除受管 venv 并返回删除事实。
func (s *EnvironmentService) RebuildDependencies(
	ctx context.Context,
	request DependenciesRequest,
) (DependenciesResult, error) {
	if ctx == nil || s == nil || s.dependencies == nil {
		return DependenciesResult{}, errors.New("dependency rebuild request is invalid")
	}
	return s.dependencies.Rebuild(ctx, request)
}

// Repair 重新校验 uv、准备受管 Python、重建 venv 并执行 locked sync。
func (s *EnvironmentService) Repair(
	ctx context.Context,
	request EnvironmentRequest,
) (EnvironmentResult, error) {
	if ctx == nil || s == nil || s.uv == nil || s.python == nil || s.dependencies == nil {
		return EnvironmentResult{}, errors.New("environment repair request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return EnvironmentResult{}, err
	}
	uvExecutable, err := s.ensureUVForRepair(ctx, request)
	if err != nil {
		return EnvironmentResult{}, err
	}
	pythonResult, err := s.PreparePython(ctx, PythonRequest{
		ProjectDir:       request.ProjectDir,
		PythonInstallDir: request.PythonInstallDir,
		ProjectEnvDir:    request.ProjectEnvDir,
		CacheDir:         request.CacheDir,
		Branch:           request.Branch,
		Commit:           request.Commit,
		Line:             request.Line,
	})
	if err != nil {
		return EnvironmentResult{}, err
	}
	dependencyRequest := DependenciesRequest{
		ProjectDir:    request.ProjectDir,
		ProjectEnvDir: request.ProjectEnvDir,
		PythonVersion: pythonResult.Spec.Version.String(),
		OperationID:   request.OperationID,
		Branch:        request.Branch,
		Commit:        request.Commit,
		Line:          request.Line,
	}
	if _, err := s.RebuildDependencies(ctx, dependencyRequest); err != nil {
		return EnvironmentResult{}, err
	}
	dependencyResult, err := s.SyncDependencies(ctx, dependencyRequest)
	if err != nil {
		return EnvironmentResult{}, err
	}
	return EnvironmentResult{
		UVExecutable: uvExecutable,
		UVReady:      true,
		Python:       pythonResult.Spec,
		Dependencies: dependencyResult,
	}, nil
}

// RepairEnvironment 只修复 uv 与受管 Python，不删除或同步项目 venv。
func (s *EnvironmentService) RepairEnvironment(
	ctx context.Context,
	request EnvironmentRequest,
) (EnvironmentResult, error) {
	if ctx == nil || s == nil || s.uv == nil || s.python == nil {
		return EnvironmentResult{}, errors.New("environment repair request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return EnvironmentResult{}, err
	}
	uvExecutable, err := s.ensureUVForRepair(ctx, request)
	if err != nil {
		return EnvironmentResult{}, err
	}
	pythonResult, err := s.PreparePython(ctx, PythonRequest{
		ProjectDir:       request.ProjectDir,
		PythonInstallDir: request.PythonInstallDir,
		ProjectEnvDir:    request.ProjectEnvDir,
		CacheDir:         request.CacheDir,
		Branch:           request.Branch,
		Commit:           request.Commit,
		Line:             request.Line,
	})
	if err != nil {
		return EnvironmentResult{}, err
	}
	return EnvironmentResult{
		UVExecutable: uvExecutable,
		UVReady:      true,
		Python:       pythonResult.Spec,
	}, nil
}

func (s *EnvironmentService) ensureUVForRepair(
	ctx context.Context,
	request EnvironmentRequest,
) (string, error) {
	if repair, ok := s.uv.(UVRepairOperations); ok {
		if withLine, ok := s.uv.(interface {
			RepairWithLine(context.Context, string, mirror.Policy, LineFunc) (string, error)
		}); ok {
			return withLine.RepairWithLine(ctx, request.OperationID, request.BootstrapPolicy, request.Line)
		}
		return repair.Repair(ctx, request.OperationID, request.BootstrapPolicy)
	}
	return s.ensureUV(ctx, request.OperationID, request.BootstrapPolicy, request.Line)
}

var _ UVOperations = (*Bootstrapper)(nil)
var _ UVRepairOperations = (*Bootstrapper)(nil)
var _ PythonOperations = (*PythonService)(nil)
var _ DependencyOperations = (*DependenciesService)(nil)
