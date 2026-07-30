package filesystem

import (
	"context"
	"time"
)

// RenameKind 限制原子重命名可接受的迁移类别。
type RenameKind string

const (
	RenameRepositoryToRetired RenameKind = "repository_to_retired"
	RenameUpdateToRepository  RenameKind = "update_to_repository"
	RenameUVStagingToVersion  RenameKind = "uv_staging_to_version"
)

func (k RenameKind) String() string { return string(k) }

func (k RenameKind) Valid() bool {
	switch k {
	case RenameRepositoryToRetired,
		RenameUpdateToRepository,
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
}

// RenameResult 报告重命名副作用是否已经生效。
type RenameResult struct {
	MutationApplied bool
}

// WaitFunc 为有限重试提供可取消的等待实现。
type WaitFunc func(ctx context.Context, delay time.Duration) error
