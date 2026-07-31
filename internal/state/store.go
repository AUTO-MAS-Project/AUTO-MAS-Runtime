package state

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

type snapshotOwner struct {
	marker byte
}

// Store 线性化状态构造、文件 I/O 和关闭。
type Store struct {
	mu sync.Mutex // 保护 clock、files、closed、closeErr，并覆盖全部 I/O。

	layout        *config.Layout
	files         stateFiles
	owner         *snapshotOwner
	clock         func() time.Time
	marshalIndent marshalIndentFunc
	closed        bool
	closeErr      error
}

// NewStore 创建并拥有 T2.5 StateFiles。
func NewStore(
	ctx context.Context,
	layout *config.Layout,
	options ...Option,
) (*Store, error) {
	return newStoreWithDependencies(
		ctx,
		layout,
		storeDependencies{
			openFiles:     newFilesystemStateFiles,
			marshalIndent: json.MarshalIndent,
		},
		options...,
	)
}

func newStoreWithDependencies(
	ctx context.Context,
	layout *config.Layout,
	dependencies storeDependencies,
	optionValues ...Option,
) (*Store, error) {
	if ctx == nil {
		return nil, validationError("ctx")
	}
	if layout == nil {
		return nil, validationError("layout")
	}
	if dependencies.openFiles == nil {
		return nil, validationError("openFiles")
	}
	if dependencies.marshalIndent == nil {
		return nil, validationError("marshalIndent")
	}
	values, err := applyOptions(optionValues...)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	files, err := dependencies.openFiles(ctx, layout)
	if err != nil {
		return nil, err
	}
	if files == nil {
		return nil, validationError("files")
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, files.Close())
	}
	return &Store{
		layout:        layout,
		files:         files,
		owner:         &snapshotOwner{marker: 1},
		clock:         values.clock,
		marshalIndent: dependencies.marshalIndent,
	}, nil
}

// Close 幂等关闭 Store 拥有的 StateFiles，并缓存第一次结果。
func (s *Store) Close() error {
	if s == nil {
		return ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	s.closeErr = s.files.Close()
	return s.closeErr
}

func (s *Store) sampleTimeLocked(field string) (time.Time, error) {
	now := s.clock().UTC().Round(0)
	if err := validateTimestamp(field, now); err != nil {
		return time.Time{}, err
	}
	return now, nil
}

func validateContext(ctx context.Context) error {
	if ctx == nil {
		return validationError("ctx")
	}
	return ctx.Err()
}
