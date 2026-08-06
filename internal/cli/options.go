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
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
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
