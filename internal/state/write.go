package state

import (
	"context"
	"errors"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

type transactionChangedError struct {
	cause error
}

func (e *transactionChangedError) Error() string {
	return ErrTransactionChanged.Error()
}

func (e *transactionChangedError) Unwrap() []error {
	if e == nil || e.cause == nil {
		return []error{ErrTransactionChanged}
	}
	return []error{ErrTransactionChanged, e.cause}
}

// WriteTransaction 校验并原子发布一个完整事务状态。
func (s *Store) WriteTransaction(
	ctx context.Context,
	kind TransactionKind,
	value TransactionState,
) error {
	if s == nil {
		return ErrClosed
	}
	if err := validateContext(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateTransaction(kind, value); err != nil {
		return err
	}
	fileKind, file, _, err := transactionFile(s.layout, kind)
	if err != nil {
		return err
	}
	payload, err := s.encodeStateLocked(file, value)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := s.files.WriteAtomic(ctx, fileKind, payload)
	return classifyWriteResult(file, result, err)
}

// WriteEnvironment 校验并原子发布稳定环境状态。
func (s *Store) WriteEnvironment(
	ctx context.Context,
	value EnvironmentState,
) error {
	if s == nil {
		return ErrClosed
	}
	if err := validateContext(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateEnvironment(s.layout, value); err != nil {
		return err
	}
	const file = "environment"
	payload, err := s.encodeStateLocked(file, value)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := s.files.WriteAtomic(
		ctx,
		filesystem.StateEnvironment,
		payload,
	)
	return classifyWriteResult(file, result, err)
}

// RemoveTransaction 使用同一 Store 读取的原 token 条件删除事务。
func (s *Store) RemoveTransaction(
	ctx context.Context,
	snapshot TransactionSnapshot,
) error {
	if s == nil {
		return ErrClosed
	}
	if err := validateContext(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if snapshot.owner == nil || snapshot.owner != s.owner ||
		snapshot.snapshot == nil || !snapshot.kind.Valid() {
		return validationError("snapshot")
	}
	fileKind, file, _, err := transactionFile(s.layout, snapshot.kind)
	if err != nil {
		return err
	}
	if snapshot.snapshot.kind() != fileKind {
		return validationError("snapshot")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := s.files.RemoveTransactionIfUnchanged(
		ctx,
		snapshot.snapshot,
	)
	return classifyRemoveResult(file, result, err)
}

func (s *Store) encodeStateLocked(file string, value any) ([]byte, error) {
	encoded, err := s.marshalIndent(value, "", "  ")
	if err != nil {
		return nil, &WriteError{
			File:  file,
			Phase: WritePhaseEncode,
			Cause: err,
		}
	}
	if int64(len(encoded)) >= maxStateFileBytes {
		return nil, &WriteError{
			File:  file,
			Phase: WritePhaseEncode,
			Cause: errStateFileTooLarge,
		}
	}
	payload := make([]byte, len(encoded)+1)
	copy(payload, encoded)
	payload[len(payload)-1] = '\n'
	schemaVersion, err := inspectStateJSON(payload)
	if err != nil || schemaVersion != SchemaVersion {
		return nil, &WriteError{
			File:  file,
			Phase: WritePhaseEncode,
			Cause: errInvalidJSON,
		}
	}
	return payload, nil
}

func classifyWriteResult(
	file string,
	result writeResult,
	err error,
) error {
	if err == nil {
		if result.mutationApplied && !result.recoveryRequired {
			return nil
		}
		return &WriteError{
			File:             file,
			Phase:            WritePhaseWrite,
			MutationApplied:  result.mutationApplied,
			RecoveryRequired: result.recoveryRequired,
			Cause:            errMutationNotApplied,
		}
	}

	var filesystemWriteErr *filesystem.StateWriteError
	if errors.As(err, &filesystemWriteErr) {
		phase, known := filesystemStateWritePhase(filesystemWriteErr.Phase)
		cause := filesystemWriteErr.Cause
		if !known || cause == nil {
			phase = WritePhaseWrite
			cause = err
		}
		return &WriteError{
			File:  file,
			Phase: phase,
			MutationApplied: result.mutationApplied ||
				filesystemWriteErr.MutationApplied,
			RecoveryRequired: result.recoveryRequired ||
				filesystemWriteErr.RecoveryRequired,
			Cause:        cause,
			CleanupError: filesystemWriteErr.CleanupError,
		}
	}
	if !result.mutationApplied &&
		!result.recoveryRequired &&
		(errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	return &WriteError{
		File:             file,
		Phase:            WritePhaseWrite,
		MutationApplied:  result.mutationApplied,
		RecoveryRequired: result.recoveryRequired,
		Cause:            err,
	}
}

func classifyRemoveResult(
	file string,
	result removeResult,
	err error,
) error {
	if err == nil {
		if !result.recoveryRequired {
			return nil
		}
		return &WriteError{
			File:             file,
			Phase:            WritePhaseRemove,
			MutationApplied:  result.mutationApplied,
			RecoveryRequired: true,
			Cause:            filesystem.ErrStateRecoveryRequired,
		}
	}

	if !result.mutationApplied && !result.recoveryRequired &&
		errors.Is(err, filesystem.ErrIdentityChanged) {
		return &transactionChangedError{cause: err}
	}
	if !result.mutationApplied && !result.recoveryRequired &&
		(errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded)) {
		return err
	}

	cause := err
	var cleanup error
	var removeErr *filesystem.StateRemoveError
	if errors.As(err, &removeErr) {
		if removeErr.Cause != nil {
			cause = removeErr.Cause
		}
		cleanup = removeErr.CleanupError
	}
	if errors.Is(cause, filesystem.ErrIdentityChanged) {
		cause = &transactionChangedError{cause: cause}
	}
	return &WriteError{
		File:             file,
		Phase:            WritePhaseRemove,
		MutationApplied:  result.mutationApplied,
		RecoveryRequired: result.recoveryRequired,
		Cause:            cause,
		CleanupError:     cleanup,
	}
}
