package state

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

type stateFileSnapshot interface {
	kind() filesystem.StateFileKind
	bytes() []byte
	stateFileSnapshot()
}

type writeResult struct {
	mutationApplied  bool
	recoveryRequired bool
}

type removeResult struct {
	mutationApplied  bool
	recoveryRequired bool
}

type stateFiles interface {
	Read(
		ctx context.Context,
		kind filesystem.StateFileKind,
		maxBytes int64,
	) (stateFileSnapshot, error)
	WriteAtomic(
		ctx context.Context,
		kind filesystem.StateFileKind,
		payload []byte,
	) (writeResult, error)
	RemoveTransactionIfUnchanged(
		ctx context.Context,
		snapshot stateFileSnapshot,
	) (removeResult, error)
	Close() error
}

type stateFilesFactory func(
	ctx context.Context,
	layout *config.Layout,
) (stateFiles, error)

type marshalIndentFunc func(
	value any,
	prefix string,
	indent string,
) ([]byte, error)

type storeDependencies struct {
	openFiles     stateFilesFactory
	marshalIndent marshalIndentFunc
}

type filesystemStateFiles struct {
	files *filesystem.StateFiles
}

type filesystemStateFileSnapshot struct {
	snapshot filesystem.StateFileSnapshot
}

func newFilesystemStateFiles(
	ctx context.Context,
	layout *config.Layout,
) (stateFiles, error) {
	files, err := filesystem.NewStateFiles(ctx, layout)
	if err != nil {
		return nil, err
	}
	return &filesystemStateFiles{files: files}, nil
}

func (f *filesystemStateFiles) Read(
	ctx context.Context,
	kind filesystem.StateFileKind,
	maxBytes int64,
) (stateFileSnapshot, error) {
	snapshot, err := f.files.Read(ctx, kind, maxBytes)
	if err != nil {
		return nil, err
	}
	return filesystemStateFileSnapshot{snapshot: snapshot}, nil
}

func (f *filesystemStateFiles) WriteAtomic(
	ctx context.Context,
	kind filesystem.StateFileKind,
	payload []byte,
) (writeResult, error) {
	result, err := f.files.WriteAtomic(ctx, kind, payload)
	return normalizeFilesystemWriteResult(result, err)
}

func (f *filesystemStateFiles) RemoveTransactionIfUnchanged(
	ctx context.Context,
	snapshot stateFileSnapshot,
) (removeResult, error) {
	retained, ok := snapshot.(filesystemStateFileSnapshot)
	if !ok {
		return removeResult{}, filesystem.ErrInvalidToken
	}
	result, err := f.files.RemoveTransactionIfUnchanged(ctx, retained.snapshot)
	return normalizeFilesystemRemoveResult(result, err)
}

func (f *filesystemStateFiles) Close() error {
	return f.files.Close()
}

func (s filesystemStateFileSnapshot) kind() filesystem.StateFileKind {
	return s.snapshot.Kind()
}

func (s filesystemStateFileSnapshot) bytes() []byte {
	return s.snapshot.Bytes()
}

func (filesystemStateFileSnapshot) stateFileSnapshot() {}

func normalizeFilesystemWriteResult(
	result filesystem.WriteAtomicResult,
	err error,
) (writeResult, error) {
	normalizedResult := writeResult{
		mutationApplied:  result.MutationApplied,
		recoveryRequired: result.RecoveryRequired,
	}
	if err == nil {
		return normalizedResult, nil
	}

	var writeErr *filesystem.StateWriteError
	if errors.As(err, &writeErr) {
		normalizedResult.mutationApplied = normalizedResult.mutationApplied ||
			writeErr.MutationApplied
		normalizedResult.recoveryRequired = normalizedResult.recoveryRequired ||
			writeErr.RecoveryRequired
	}
	return normalizedResult, err
}

func normalizeFilesystemRemoveResult(
	result filesystem.StateRemoveResult,
	err error,
) (removeResult, error) {
	return removeResult{
		mutationApplied:  result.MutationApplied,
		recoveryRequired: result.RecoveryRequired,
	}, err
}

func filesystemStateWritePhase(
	phase filesystem.StateWritePhase,
) (WritePhase, bool) {
	switch phase {
	case filesystem.StateWritePhaseRecover:
		return WritePhaseRecover, true
	case filesystem.StateWritePhaseCreate:
		return WritePhaseCreate, true
	case filesystem.StateWritePhaseWrite:
		return WritePhaseWrite, true
	case filesystem.StateWritePhaseSync:
		return WritePhaseSync, true
	case filesystem.StateWritePhaseRename:
		return WritePhaseRename, true
	case filesystem.StateWritePhaseFinalize:
		return WritePhaseFinalize, true
	case filesystem.StateWritePhaseClose:
		return WritePhaseClose, true
	default:
		return WritePhaseWrite, false
	}
}
