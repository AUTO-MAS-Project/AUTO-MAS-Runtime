package filesystem

import (
	"context"
	"time"
)

// WithWait 注入可取消的有限重试等待实现。
func WithWait(wait WaitFunc) Option {
	return func(options *options) error {
		if options == nil || wait == nil || options.waitSet {
			return ErrInvalidArgument
		}
		options.wait = wait
		options.waitSet = true
		return nil
	}
}

// WithRenameDelays 配置有限重试的等待序列。
func WithRenameDelays(delays ...time.Duration) Option {
	copied := append([]time.Duration(nil), delays...)
	return func(options *options) error {
		if options == nil || options.delaysSet ||
			len(copied) == 0 || len(copied) > 16 {
			return ErrInvalidArgument
		}
		for _, delay := range copied {
			if delay <= 0 {
				return ErrInvalidArgument
			}
		}
		options.delays = append([]time.Duration(nil), copied...)
		options.delaysSet = true
		return nil
	}
}

func defaultRenameDelays() []time.Duration {
	return []time.Duration{
		50 * time.Millisecond,
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
	}
}

func defaultWait(ctx context.Context, delay time.Duration) error {
	if ctx == nil || delay <= 0 {
		return ErrInvalidArgument
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RenameKind 限制原子重命名可接受的迁移类别。
type RenameKind string

const (
	RenameRepositoryToRetired RenameKind = "repository_to_retired"
	RenameUpdateToRepository  RenameKind = "update_to_repository"
	RenameRepositoryRollback  RenameKind = "repository_rollback"
	RenameUVStagingToVersion  RenameKind = "uv_staging_to_version"
)

func (k RenameKind) String() string { return string(k) }

func (k RenameKind) Valid() bool {
	switch k {
	case RenameRepositoryToRetired,
		RenameUpdateToRepository,
		RenameRepositoryRollback,
		RenameUVStagingToVersion:
		return true
	default:
		return false
	}
}

// RenameRequest 描述一次受控原子重命名请求。
type RenameRequest struct {
	Kind        RenameKind
	Source      string
	Destination string
	OperationID string
	Version     string
	Reason      string
	// ExpectedSourceIdentity 可选地约束源目录必须仍是分类时的同一目录叶子。
	ExpectedSourceIdentity *DirectoryIdentity
}

// RenameResult 报告重命名副作用是否已经生效。
type RenameResult struct {
	MutationApplied bool
}

// WaitFunc 为有限重试提供可取消的等待实现。
type WaitFunc func(ctx context.Context, delay time.Duration) error
