package uv

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

// ProductionEnvironment 把固定 uv 与共享 UVRunner 绑定到 Python/依赖服务。
// 构造本身不访问网络；Ensure/Check/Repair 才执行各自声明的副作用。
type ProductionEnvironment struct {
	layout    *config.Layout
	bootstrap *Bootstrapper
}

// NewProductionEnvironment 创建 Windows 首版使用的受管环境适配器。
func NewProductionEnvironment(layout *config.Layout) (*ProductionEnvironment, error) {
	if layout == nil {
		return nil, errors.New("production environment layout is invalid")
	}
	bootstrap, err := NewBootstrapper(layout)
	if err != nil {
		return nil, err
	}
	return &ProductionEnvironment{layout: layout, bootstrap: bootstrap}, nil
}

// Ensure 按固定顺序准备 uv、Python 和锁定依赖。
func (s *ProductionEnvironment) Ensure(
	ctx context.Context,
	request EnvironmentRequest,
) (EnvironmentResult, error) {
	if s == nil || s.layout == nil || s.bootstrap == nil {
		return EnvironmentResult{}, errors.New("production environment is invalid")
	}
	uvExecutable, err := s.bootstrap.EnsureWithLine(ctx, request.OperationID, request.BootstrapPolicy, request.Line)
	if err != nil {
		return EnvironmentResult{}, err
	}
	service, err := s.services(uvExecutable, request)
	if err != nil {
		return EnvironmentResult{}, err
	}
	return service.Ensure(ctx, request)
}

// EnsureUV 准备固定版本 uv，并返回已验证的可执行文件路径。
func (s *ProductionEnvironment) EnsureUV(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
) (string, error) {
	return s.ensureUV(ctx, operationID, policy, nil)
}

// EnsureUVWithLine 准备固定版本 uv，并转发版本检查输出。
func (s *ProductionEnvironment) EnsureUVWithLine(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
	line LineFunc,
) (string, error) {
	return s.ensureUV(ctx, operationID, policy, line)
}

func (s *ProductionEnvironment) ensureUV(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
	line LineFunc,
) (string, error) {
	if s == nil || s.bootstrap == nil {
		return "", errors.New("production environment is invalid")
	}
	return s.bootstrap.EnsureWithLine(ctx, operationID, policy, line)
}

// RepairUV 删除固定版本 uv 受管事实并重新下载校验。
func (s *ProductionEnvironment) RepairUV(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
) (string, error) {
	return s.repairUV(ctx, operationID, policy, nil)
}

// RepairUVWithLine 删除固定版本 uv 受管事实并重新下载校验，同时转发输出。
func (s *ProductionEnvironment) RepairUVWithLine(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
	line LineFunc,
) (string, error) {
	return s.repairUV(ctx, operationID, policy, line)
}

func (s *ProductionEnvironment) repairUV(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
	line LineFunc,
) (string, error) {
	if s == nil || s.bootstrap == nil {
		return "", errors.New("production environment is invalid")
	}
	return s.bootstrap.RepairWithLine(ctx, operationID, policy, line)
}

// CheckUV 只检查固定版本 uv，不触碰网络或创建目录。
func (s *ProductionEnvironment) CheckUV(ctx context.Context) (bool, error) {
	return s.checkUV(ctx, nil)
}

// CheckUVWithLine 只检查固定版本 uv，并转发版本检查输出。
func (s *ProductionEnvironment) CheckUVWithLine(ctx context.Context, line LineFunc) (bool, error) {
	return s.checkUV(ctx, line)
}

func (s *ProductionEnvironment) checkUV(ctx context.Context, line LineFunc) (bool, error) {
	if s == nil || s.bootstrap == nil {
		return false, errors.New("production environment is invalid")
	}
	return s.bootstrap.CheckWithLine(ctx, line)
}

// ReadPythonSpec 只读取并校验项目声明的精确 Python 契约。
func (s *ProductionEnvironment) ReadPythonSpec(ctx context.Context, projectDir string) (PythonSpec, error) {
	if s == nil || s.layout == nil {
		return PythonSpec{}, errors.New("production environment is invalid")
	}
	return ReadPythonSpec(ctx, s.layout, projectDir)
}

// PreparePython 准备项目声明的精确受管 Python。
func (s *ProductionEnvironment) PreparePython(
	ctx context.Context,
	request PythonRequest,
) (PythonResult, error) {
	if s == nil || s.layout == nil || s.bootstrap == nil {
		return PythonResult{}, errors.New("production environment is invalid")
	}
	service, err := s.servicesForPython(request)
	if err != nil {
		return PythonResult{}, err
	}
	return service.PreparePython(ctx, request)
}

// CheckPython 只读取项目 Python 契约和已安装事实。
func (s *ProductionEnvironment) CheckPython(
	ctx context.Context,
	request PythonRequest,
) (PythonCheckResult, error) {
	if s == nil || s.layout == nil || s.bootstrap == nil {
		return PythonCheckResult{}, errors.New("production environment is invalid")
	}
	service, err := s.servicesForPython(request)
	if err != nil {
		return PythonCheckResult{}, err
	}
	return service.CheckPython(ctx, request)
}

// SyncDependencies 执行锁定依赖同步。
func (s *ProductionEnvironment) SyncDependencies(
	ctx context.Context,
	request DependenciesRequest,
) (DependenciesResult, error) {
	if s == nil || s.layout == nil || s.bootstrap == nil {
		return DependenciesResult{}, errors.New("production environment is invalid")
	}
	service, err := s.servicesForDependencies(request)
	if err != nil {
		return DependenciesResult{}, err
	}
	return service.SyncDependencies(ctx, request)
}

// CheckDependencies 只检查锁文件与 lock 一致性。
func (s *ProductionEnvironment) CheckDependencies(
	ctx context.Context,
	request DependenciesRequest,
) (DependenciesResult, error) {
	if s == nil || s.layout == nil || s.bootstrap == nil {
		return DependenciesResult{}, errors.New("production environment is invalid")
	}
	service, err := s.servicesForDependencies(request)
	if err != nil {
		return DependenciesResult{}, err
	}
	return service.CheckDependencies(ctx, request)
}

// RebuildDependencies 只删除受管 venv。
func (s *ProductionEnvironment) RebuildDependencies(
	ctx context.Context,
	request DependenciesRequest,
) (DependenciesResult, error) {
	if s == nil || s.layout == nil || s.bootstrap == nil {
		return DependenciesResult{}, errors.New("production environment is invalid")
	}
	service, err := s.servicesForDependencies(request)
	if err != nil {
		return DependenciesResult{}, err
	}
	return service.RebuildDependencies(ctx, request)
}

// Check 只读取固定 uv、Python 契约和锁文件状态。
func (s *ProductionEnvironment) Check(
	ctx context.Context,
	request EnvironmentRequest,
) (EnvironmentResult, error) {
	if s == nil || s.layout == nil || s.bootstrap == nil {
		return EnvironmentResult{}, errors.New("production environment is invalid")
	}
	ready, err := s.bootstrap.CheckWithLine(ctx, request.Line)
	if err != nil {
		return EnvironmentResult{}, err
	}
	if !ready {
		return EnvironmentResult{UVReady: false}, nil
	}
	uvExecutable, err := s.layout.UVExecutable(FixedVersion)
	if err != nil {
		return EnvironmentResult{}, err
	}
	service, err := s.services(uvExecutable, request)
	if err != nil {
		return EnvironmentResult{}, err
	}
	return service.Check(ctx, request)
}

// Repair 重新取得 uv、准备受管 Python 并重建 managed venv。
func (s *ProductionEnvironment) Repair(
	ctx context.Context,
	request EnvironmentRequest,
) (EnvironmentResult, error) {
	if s == nil || s.layout == nil || s.bootstrap == nil {
		return EnvironmentResult{}, errors.New("production environment is invalid")
	}
	uvExecutable, err := s.bootstrap.RepairWithLine(ctx, request.OperationID, request.BootstrapPolicy, request.Line)
	if err != nil {
		return EnvironmentResult{}, err
	}
	service, err := s.services(uvExecutable, request)
	if err != nil {
		return EnvironmentResult{}, err
	}
	return service.Repair(ctx, request)
}

// RepairEnvironment 重新取得 uv 并准备受管 Python，但保留现有项目 venv。
func (s *ProductionEnvironment) RepairEnvironment(
	ctx context.Context,
	request EnvironmentRequest,
) (EnvironmentResult, error) {
	if s == nil || s.layout == nil || s.bootstrap == nil {
		return EnvironmentResult{}, errors.New("production environment is invalid")
	}
	uvExecutable, err := s.bootstrap.RepairWithLine(ctx, request.OperationID, request.BootstrapPolicy, request.Line)
	if err != nil {
		return EnvironmentResult{}, err
	}
	service, err := s.services(uvExecutable, request)
	if err != nil {
		return EnvironmentResult{}, err
	}
	return service.RepairEnvironment(ctx, request)
}

func (s *ProductionEnvironment) services(
	uvExecutable string,
	request EnvironmentRequest,
) (*EnvironmentService, error) {
	if s == nil || s.layout == nil {
		return nil, errors.New("production environment is invalid")
	}
	projectDir := request.ProjectDir
	if projectDir == "" {
		projectDir = s.layout.RepoDir()
	}
	projectEnvDir := request.ProjectEnvDir
	if projectEnvDir == "" {
		projectEnvDir = s.layout.VenvDir()
	}
	pythonInstallDir := request.PythonInstallDir
	if pythonInstallDir == "" {
		pythonInstallDir = s.layout.PythonDir()
	}
	cacheDir := request.CacheDir
	if cacheDir == "" {
		cacheDir = s.layout.UVCacheDir()
	}
	runner, err := NewRunner(RunnerConfig{
		Executable:       uvExecutable,
		ProjectDir:       projectDir,
		PythonInstallDir: pythonInstallDir,
		ProjectEnvDir:    projectEnvDir,
		CacheDir:         cacheDir,
	})
	if err != nil {
		return nil, err
	}
	python, err := NewPythonService(s.layout, runner)
	if err != nil {
		return nil, err
	}
	dependencies, err := NewDependenciesService(
		s.layout,
		runner,
		filesystemVersionRemover{layout: s.layout},
	)
	if err != nil {
		return nil, err
	}
	return NewEnvironmentService(verifiedUV{path: uvExecutable}, python, dependencies)
}

func (s *ProductionEnvironment) servicesForPython(
	request PythonRequest,
) (*EnvironmentService, error) {
	if s == nil || s.layout == nil {
		return nil, errors.New("production environment is invalid")
	}
	return s.services(s.uvExecutable(), EnvironmentRequest{
		ProjectDir:       request.ProjectDir,
		ProjectEnvDir:    request.ProjectEnvDir,
		PythonInstallDir: request.PythonInstallDir,
		CacheDir:         request.CacheDir,
		Branch:           request.Branch,
		Commit:           request.Commit,
		BootstrapPolicy:  request.MirrorPolicy,
		Line:             request.Line,
	})
}

func (s *ProductionEnvironment) servicesForDependencies(
	request DependenciesRequest,
) (*EnvironmentService, error) {
	if s == nil || s.layout == nil {
		return nil, errors.New("production environment is invalid")
	}
	return s.services(s.uvExecutable(), EnvironmentRequest{
		ProjectDir:      request.ProjectDir,
		ProjectEnvDir:   request.ProjectEnvDir,
		Branch:          request.Branch,
		Commit:          request.Commit,
		BootstrapPolicy: request.MirrorPolicy,
		Line:            request.Line,
	})
}

func (s *ProductionEnvironment) uvExecutable() string {
	if s == nil || s.layout == nil {
		return ""
	}
	path, _ := s.layout.UVExecutable(FixedVersion)
	return path
}

type verifiedUV struct{ path string }

func (u verifiedUV) Ensure(context.Context, string, mirror.Policy) (string, error) {
	return u.path, nil
}

func (u verifiedUV) Check(context.Context) (bool, error) { return u.path != "", nil }

var _ interface {
	Ensure(context.Context, EnvironmentRequest) (EnvironmentResult, error)
	Check(context.Context, EnvironmentRequest) (EnvironmentResult, error)
	Repair(context.Context, EnvironmentRequest) (EnvironmentResult, error)
} = (*ProductionEnvironment)(nil)
