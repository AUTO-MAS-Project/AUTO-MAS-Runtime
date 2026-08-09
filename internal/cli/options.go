package cli

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/doctor"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/logging"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/version"
)

// versionSourceFunc 是 version 命令与 hello 版本字段的来源。
type versionSourceFunc func(context.Context) (version.Info, error)

// WithVersionSource 注入版本信息来源，默认使用 version.Load。
func WithVersionSource(source versionSourceFunc) Option {
	return func(values *options) error {
		if source == nil {
			return errors.New("cli version source must not be nil")
		}
		values.versionSource = source
		return nil
	}
}

// doctorService 是 cli 消费的 doctor 服务窄接口。
type doctorService interface {
	Run(ctx context.Context, emitter *protocol.Emitter) (doctor.Report, error)
}

// doctorFactory 构造 doctor 服务；生产默认返回真实 Service。
type doctorFactory func(layout *config.Layout, probes doctor.Probes) (doctorService, error)

// WithDoctorFactory 注入 doctor 服务工厂，主要供契约测试替换服务替身。
func WithDoctorFactory(factory doctorFactory) Option {
	return func(values *options) error {
		if factory == nil {
			return errors.New("cli doctor factory must not be nil")
		}
		values.doctorFactory = factory
		return nil
	}
}

// workspaceService 是 cli 消费的 workspace 应用服务窄接口。
type workspaceService interface {
	Check(ctx context.Context) (gitrepo.CheckResult, error)
	Sync(ctx context.Context, request gitrepo.SyncRequest) (gitrepo.SyncResult, error)
}

// workspaceFactory 构造只在调用方法时产生副作用的 workspace 服务。
type workspaceFactory func(layout *config.Layout) (workspaceService, error)

// WithWorkspaceFactory 注入 workspace 服务工厂，供命令与契约测试隔离真实 Git/Mutex。
func WithWorkspaceFactory(factory workspaceFactory) Option {
	return func(values *options) error {
		if factory == nil {
			return errors.New("cli workspace factory must not be nil")
		}
		values.workspaceFactory = factory
		return nil
	}
}

// environmentService 是 M5 命令消费的环境编排窄接口。
type environmentService interface {
	Ensure(context.Context, uv.EnvironmentRequest) (uv.EnvironmentResult, error)
	Check(context.Context, uv.EnvironmentRequest) (uv.EnvironmentResult, error)
	Repair(context.Context, uv.EnvironmentRequest) (uv.EnvironmentResult, error)
	RepairEnvironment(context.Context, uv.EnvironmentRequest) (uv.EnvironmentResult, error)
	EnsureUV(context.Context, string, mirror.Policy) (string, error)
	RepairUV(context.Context, string, mirror.Policy) (string, error)
	CheckUV(context.Context) (bool, error)
	ReadPythonSpec(context.Context, string) (uv.PythonSpec, error)
	PreparePython(context.Context, uv.PythonRequest) (uv.PythonResult, error)
	CheckPython(context.Context, uv.PythonRequest) (uv.PythonCheckResult, error)
	SyncDependencies(context.Context, uv.DependenciesRequest) (uv.DependenciesResult, error)
	CheckDependencies(context.Context, uv.DependenciesRequest) (uv.DependenciesResult, error)
	RebuildDependencies(context.Context, uv.DependenciesRequest) (uv.DependenciesResult, error)
}

type environmentFactory func(layout *config.Layout) (environmentService, error)

// environmentStateStore 是 M5 需要的最小稳定状态读写能力。
type environmentStateStore interface {
	ReadEnvironment(context.Context) (state.EnvironmentState, error)
	NewReadyEnvironment(string, string) (state.EnvironmentState, error)
	NewBrokenEnvironment(state.Revision, state.BrokenEnvironment) (state.EnvironmentState, error)
	WriteEnvironment(context.Context, state.EnvironmentState) error
	NewTransaction(state.TransactionKind, state.TransactionInput) (state.TransactionState, error)
	WriteTransaction(context.Context, state.TransactionKind, state.TransactionState) error
	ReadTransaction(context.Context, state.TransactionKind) (state.TransactionSnapshot, error)
	RemoveTransaction(context.Context, state.TransactionSnapshot) error
	Close() error
}

type environmentStateStoreFactory func(
	context.Context,
	*config.Layout,
	func() time.Time,
) (environmentStateStore, error)

type mutationCoordinatorFactory func(
	context.Context,
	*config.Layout,
) (gitrepo.MutationCoordinator, error)

// WithEnvironmentFactory 注入 M5 环境服务，供命令测试隔离真实 uv/Python。
func WithEnvironmentFactory(factory environmentFactory) Option {
	return func(values *options) error {
		if factory == nil {
			return errors.New("cli environment factory must not be nil")
		}
		values.environmentFactory = factory
		return nil
	}
}

// WithEnvironmentStateStoreFactory 注入 M5 稳定状态存储。
func WithEnvironmentStateStoreFactory(factory environmentStateStoreFactory) Option {
	return func(values *options) error {
		if factory == nil {
			return errors.New("cli environment state store factory must not be nil")
		}
		values.environmentStateStoreFactory = factory
		return nil
	}
}

// WithMutationCoordinatorFactory 注入 M5 mutation 锁协调器。
func WithMutationCoordinatorFactory(factory mutationCoordinatorFactory) Option {
	return func(values *options) error {
		if factory == nil {
			return errors.New("cli mutation coordinator factory must not be nil")
		}
		values.mutationCoordinatorFactory = factory
		return nil
	}
}

// workspaceLogger 是本次同步日志与删除审计共用的窄能力。
type workspaceLogger interface {
	gitrepo.OperationLogger
	Record(
		ctx context.Context,
		level logging.Level,
		message string,
		details map[string]any,
	) (logging.WriteResult, error)
}

type workspaceLoggerFactory func(
	ctx context.Context,
	layout *config.Layout,
	stderr io.Writer,
	command string,
	operationID string,
	clock func() time.Time,
) (workspaceLogger, error)

// WithWorkspaceLoggerFactory 注入 Runtime logger 工厂，主要供取消与契约测试使用。
func WithWorkspaceLoggerFactory(factory workspaceLoggerFactory) Option {
	return func(values *options) error {
		if factory == nil {
			return errors.New("cli workspace logger factory must not be nil")
		}
		values.workspaceLoggerFactory = factory
		return nil
	}
}

var _ filesystem.Auditor = (*workspaceLogBinding)(nil)
