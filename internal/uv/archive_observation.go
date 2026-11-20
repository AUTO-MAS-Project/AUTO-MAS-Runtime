package uv

import (
	"context"
	"time"
)

// ArchiveObservation 保存本次串行准备操作的归档事实，随终态在本机返回桌面。
type ArchiveObservation struct {
	clock                    func() time.Time
	Cached                   *bool
	Verify, Unpack, Download *time.Duration
}
type archiveObservationKey struct{}

// NewArchiveObservation 使用调用方提供的时钟测量准备阶段。
func NewArchiveObservation(clock func() time.Time) *ArchiveObservation {
	return &ArchiveObservation{clock: clock}
}

// WithArchiveObservation 由已协商启动诊断能力的命令设置。
func WithArchiveObservation(ctx context.Context, observation *ArchiveObservation) context.Context {
	return context.WithValue(ctx, archiveObservationKey{}, observation)
}
func (o *ArchiveObservation) measure(target string, run func() error) error {
	if o == nil {
		return run()
	}
	start := o.clock()
	err := run()
	elapsed := o.clock().Sub(start)
	if target == "verify" {
		o.Verify = &elapsed
	} else {
		o.Unpack = &elapsed
	}
	return err
}
