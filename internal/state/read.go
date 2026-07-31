package state

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

// TransactionSnapshot 把已校验事务值绑定到 T2.5 原始条件删除 token。
type TransactionSnapshot struct {
	owner    *snapshotOwner
	kind     TransactionKind
	state    TransactionState
	snapshot stateFileSnapshot
}

// Kind 返回 snapshot 对应的事务类型。
func (s TransactionSnapshot) Kind() TransactionKind {
	return s.kind
}

// State 返回已校验事务值的副本。
func (s TransactionSnapshot) State() TransactionState {
	return s.state
}

// ReadTransaction 严格读取并保留不可重建的事务 snapshot。
func (s *Store) ReadTransaction(
	ctx context.Context,
	kind TransactionKind,
) (TransactionSnapshot, error) {
	if s == nil {
		return TransactionSnapshot{}, ErrClosed
	}
	if err := validateContext(ctx); err != nil {
		return TransactionSnapshot{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return TransactionSnapshot{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return TransactionSnapshot{}, err
	}
	if !kind.Valid() {
		return TransactionSnapshot{}, validationError("kind")
	}
	fileKind, file, path, err := transactionFile(s.layout, kind)
	if err != nil {
		return TransactionSnapshot{}, err
	}
	snapshot, err := s.files.Read(ctx, fileKind, maxStateFileBytes)
	if err != nil {
		return TransactionSnapshot{}, classifyReadError(file, path, err)
	}
	if err := ctx.Err(); err != nil {
		return TransactionSnapshot{}, err
	}
	if snapshot == nil || snapshot.kind() != fileKind {
		return TransactionSnapshot{}, &ReadError{
			File:  file,
			Cause: errInvalidValue,
		}
	}
	value, err := decodeTransaction(file, kind, snapshot.bytes())
	if err != nil {
		return TransactionSnapshot{}, err
	}
	return TransactionSnapshot{
		owner:    s.owner,
		kind:     kind,
		state:    value,
		snapshot: snapshot,
	}, nil
}

// ReadEnvironment 严格读取稳定环境状态并丢弃文件 token。
func (s *Store) ReadEnvironment(
	ctx context.Context,
) (EnvironmentState, error) {
	if s == nil {
		return EnvironmentState{}, ErrClosed
	}
	if err := validateContext(ctx); err != nil {
		return EnvironmentState{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return EnvironmentState{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return EnvironmentState{}, err
	}
	file := "environment"
	path := s.layout.EnvironmentStateFile()
	snapshot, err := s.files.Read(
		ctx,
		filesystem.StateEnvironment,
		maxStateFileBytes,
	)
	if err != nil {
		return EnvironmentState{}, classifyReadError(file, path, err)
	}
	if err := ctx.Err(); err != nil {
		return EnvironmentState{}, err
	}
	if snapshot == nil || snapshot.kind() != filesystem.StateEnvironment {
		return EnvironmentState{}, &ReadError{
			File:  file,
			Cause: errInvalidValue,
		}
	}
	return decodeEnvironment(s.layout, file, snapshot.bytes())
}

func transactionFile(
	layout *config.Layout,
	kind TransactionKind,
) (filesystem.StateFileKind, string, string, error) {
	if layout == nil {
		return "", "", "", validationError("layout")
	}
	switch kind {
	case TransactionBackend:
		return filesystem.StateBackend, kind.String(), layout.BackendStateFile(), nil
	case TransactionMutation:
		return filesystem.StateMutation, kind.String(), layout.MutationStateFile(), nil
	case TransactionUpdate:
		return filesystem.StateUpdate, kind.String(), layout.UpdateStateFile(), nil
	default:
		return "", "", "", validationError("kind")
	}
}

func classifyReadError(file string, path string, err error) error {
	if errors.Is(err, filesystem.ErrStateFileTooLarge) {
		return corrupt(file, err)
	}
	if errors.Is(err, filesystem.ErrStateFileNotFound) {
		return &NotFoundError{File: file, Path: path}
	}
	return &ReadError{File: file, Cause: err}
}
