package cli

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/doctor"
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
