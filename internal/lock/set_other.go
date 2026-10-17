//go:build !windows

package lock

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// ErrUnsupported 表示本平台尚无经过验证的跨进程锁实现。
var ErrUnsupported = errors.New("lock set is unsupported on this platform")

// Set 在原生锁协议验证前不持有平台资源。
type Set struct{}

// Lease 在原生锁协议验证前不可获取。
type Lease struct{}

// ProbeResult 保持共用的锁事实结构。
type ProbeResult struct {
	Held      bool
	Recovered bool
}

// AcquisitionResult 保持共用的获取结果结构。
type AcquisitionResult struct{}

// NewSet 不在原生锁协议缺失时创建锁文件。
func NewSet(context.Context, *config.Layout) (*Set, error) { return nil, ErrUnsupported }

// AcquireBackend 不在原生锁协议缺失时声称获取成功。
func (*Set) AcquireBackend(context.Context) (AcquisitionResult, error) {
	return AcquisitionResult{}, ErrUnsupported
}

// AcquireMutation 不在原生锁协议缺失时声称获取成功。
func (*Set) AcquireMutation(context.Context) (AcquisitionResult, error) {
	return AcquisitionResult{}, ErrUnsupported
}

// AcquireStaging 不在原生锁协议缺失时声称获取成功。
func (*Set) AcquireStaging(context.Context) (AcquisitionResult, error) {
	return AcquisitionResult{}, ErrUnsupported
}

// Probe 不将未知锁状态解释为空闲。
func (*Set) Probe(context.Context, Kind) (ProbeResult, error) { return ProbeResult{}, ErrUnsupported }

// Close 幂等收口未持有资源的占位锁集。
func (*Set) Close() error { return nil }

// Lease 返回空值，不颁发未经验证的租约。
func (AcquisitionResult) Lease() *Lease { return nil }

// Recovered 不把未知恢复状态解释为已恢复。
func (AcquisitionResult) Recovered() bool { return false }

// PeerProbe 返回空值；获取失败后调用方不得消费该结果。
func (AcquisitionResult) PeerProbe() ProbeResult { return ProbeResult{} }

// Kind 返回无效类型，不颁发未经验证的租约。
func (*Lease) Kind() Kind { return "" }

// Name 返回空值，不颁发未经验证的租约。
func (*Lease) Name() string { return "" }

// Close 幂等收口未持有资源的占位租约。
func (*Lease) Close() error { return nil }
