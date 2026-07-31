package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func newTestStoreWithMarshal(
	t *testing.T,
	files stateFiles,
	marshal marshalIndentFunc,
) *Store {
	t.Helper()
	store, err := newStoreWithDependencies(
		t.Context(),
		mustTestLayout(t),
		storeDependencies{
			openFiles: func(context.Context, *config.Layout) (stateFiles, error) {
				return files, nil
			},
			marshalIndent: marshal,
		},
		WithClock(fixedStateTime),
	)
	if err != nil {
		t.Fatalf("newStoreWithDependencies() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	return store
}

func readTestTransactionSnapshot(
	t *testing.T,
	store *Store,
	files *fakeStateFiles,
	kind TransactionKind,
) TransactionSnapshot {
	t.Helper()
	value := validTransactionState(kind)
	fileKind, _, _, err := transactionFile(store.layout, kind)
	if err != nil {
		t.Fatalf("transactionFile() error = %v", err)
	}
	files.read = func(
		context.Context,
		filesystem.StateFileKind,
		int64,
	) (stateFileSnapshot, error) {
		return fakeSnapshot(fileKind, mustStateJSON(t, value)), nil
	}
	snapshot, err := store.ReadTransaction(t.Context(), kind)
	if err != nil {
		t.Fatalf("ReadTransaction() error = %v", err)
	}
	return snapshot
}

func TestFilesystemStateFiles_NormalizesTypedWritePhases(t *testing.T) {
	t.Parallel()

	cause := errors.New("filesystem mutation failed")
	cleanup := errors.New("filesystem cleanup failed")
	tests := []struct {
		name           string
		source         filesystem.StateWritePhase
		want           WritePhase
		resultApplied  bool
		typedApplied   bool
		resultRecovery bool
		typedRecovery  bool
		wantApplied    bool
		wantRecovery   bool
	}{
		{name: "recover", source: filesystem.StateWritePhaseRecover, want: WritePhaseRecover},
		{name: "create", source: filesystem.StateWritePhaseCreate, want: WritePhaseCreate},
		{name: "write", source: filesystem.StateWritePhaseWrite, want: WritePhaseWrite},
		{name: "sync", source: filesystem.StateWritePhaseSync, want: WritePhaseSync},
		{name: "rename", source: filesystem.StateWritePhaseRename, want: WritePhaseRename},
		{name: "finalize", source: filesystem.StateWritePhaseFinalize, want: WritePhaseFinalize},
		{
			name:          "close",
			source:        filesystem.StateWritePhaseClose,
			want:          WritePhaseClose,
			resultApplied: true,
			typedRecovery: true,
			wantApplied:   true,
			wantRecovery:  true,
		},
		{
			name:           "typed_result_or",
			source:         filesystem.StateWritePhaseFinalize,
			want:           WritePhaseFinalize,
			typedApplied:   true,
			resultRecovery: true,
			wantApplied:    true,
			wantRecovery:   true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			source := &filesystem.StateWriteError{
				Phase:            test.source,
				MutationApplied:  test.typedApplied,
				RecoveryRequired: test.typedRecovery,
				Cause:            cause,
				CleanupError:     cleanup,
			}
			result, normalized := normalizeFilesystemWriteResult(
				filesystem.WriteAtomicResult{
					MutationApplied:  test.resultApplied,
					RecoveryRequired: test.resultRecovery,
				},
				source,
			)
			if result.mutationApplied != test.wantApplied ||
				result.recoveryRequired != test.wantRecovery {
				t.Fatalf(
					"normalized facts = %t/%t, want %t/%t",
					result.mutationApplied,
					result.recoveryRequired,
					test.wantApplied,
					test.wantRecovery,
				)
			}
			var retained *filesystem.StateWriteError
			if !errors.As(normalized, &retained) || retained != source {
				t.Fatalf("normalized error = %v, want original typed error", normalized)
			}
			classified := classifyWriteResult("backend", result, normalized)
			var writeErr *WriteError
			if !errors.As(classified, &writeErr) ||
				writeErr.Phase != test.want ||
				writeErr.MutationApplied != test.wantApplied ||
				writeErr.RecoveryRequired != test.wantRecovery ||
				writeErr.Code() != protocol.CodeStateWriteFailed ||
				!errors.Is(classified, cause) ||
				!errors.Is(classified, cleanup) {
				t.Fatalf("classified error = %#v, want %q typed state error", writeErr, test.want)
			}
		})
	}

	t.Run("unknown_typed_phase", func(t *testing.T) {
		source := &filesystem.StateWriteError{
			Phase:            filesystem.StateWritePhase("future"),
			MutationApplied:  true,
			RecoveryRequired: true,
			Cause:            cause,
			CleanupError:     cleanup,
		}
		result, normalized := normalizeFilesystemWriteResult(
			filesystem.WriteAtomicResult{},
			source,
		)
		classified := classifyWriteResult("backend", result, normalized)
		var writeErr *WriteError
		var retained *filesystem.StateWriteError
		if !errors.As(classified, &writeErr) ||
			writeErr.Phase != WritePhaseWrite ||
			!errors.As(classified, &retained) ||
			retained != source ||
			!writeErr.MutationApplied ||
			!writeErr.RecoveryRequired ||
			!errors.Is(classified, cause) ||
			!errors.Is(classified, cleanup) {
			t.Fatalf(
				"unknown typed normalization = %#v/%v, want write fallback/full chain",
				result,
				classified,
			)
		}
	})

	t.Run("untyped_error", func(t *testing.T) {
		result, normalized := normalizeFilesystemWriteResult(
			filesystem.WriteAtomicResult{RecoveryRequired: true},
			errors.Join(cause, cleanup),
		)
		classified := classifyWriteResult("backend", result, normalized)
		var writeErr *WriteError
		if !errors.As(classified, &writeErr) ||
			writeErr.Phase != WritePhaseWrite ||
			writeErr.MutationApplied ||
			!writeErr.RecoveryRequired ||
			!errors.Is(classified, cause) ||
			!errors.Is(classified, cleanup) {
			t.Fatalf(
				"untyped normalization = %#v/%v, want write fallback/full chain",
				result,
				classified,
			)
		}
	})

	t.Run("remove_state_remove_error", func(t *testing.T) {
		result, normalized := normalizeFilesystemRemoveResult(
			filesystem.StateRemoveResult{RecoveryRequired: true},
			&filesystem.StateRemoveError{
				Cause:        filesystem.ErrIdentityChanged,
				CleanupError: cleanup,
			},
		)
		classified := classifyRemoveResult("backend", result, normalized)
		var writeErr *WriteError
		if !errors.As(classified, &writeErr) ||
			writeErr.Phase != WritePhaseRemove ||
			writeErr.MutationApplied ||
			!writeErr.RecoveryRequired ||
			!errors.Is(classified, ErrTransactionChanged) ||
			!errors.Is(classified, cleanup) ||
			writeErr.CleanupError == nil {
			t.Fatalf(
				"remove normalization = %#v/%v, want false/true remove/full chain",
				writeErr,
				classified,
			)
		}
	})
}

func TestStore_WriteReportsCleanupFailure(t *testing.T) {
	t.Parallel()

	cause := errors.New("sync state payload")
	cleanup := errors.New("close state payload")
	files := completeFakeStateFiles()
	files.write = func(
		context.Context,
		filesystem.StateFileKind,
		[]byte,
	) (writeResult, error) {
		return writeResult{recoveryRequired: true}, &filesystem.StateWriteError{
			Phase:            filesystem.StateWritePhaseSync,
			RecoveryRequired: true,
			Cause:            cause,
			CleanupError:     cleanup,
		}
	}
	store := newTestStore(t, files, fixedStateTime)
	err := store.WriteTransaction(
		t.Context(),
		TransactionBackend,
		validTransactionState(TransactionBackend),
	)
	var writeErr *WriteError
	if !errors.As(err, &writeErr) ||
		writeErr.Phase != WritePhaseSync ||
		writeErr.MutationApplied ||
		!writeErr.RecoveryRequired ||
		writeErr.Cause != cause ||
		writeErr.CleanupError != cleanup ||
		!errors.Is(err, cause) ||
		!errors.Is(err, cleanup) {
		t.Fatalf("WriteTransaction() error = %#v, want sync recovery with both chains", writeErr)
	}
}

func TestWriteError_MapsStateWriteFailed(t *testing.T) {
	t.Parallel()

	for _, phase := range []WritePhase{
		WritePhaseEncode,
		WritePhaseRecover,
		WritePhaseCreate,
		WritePhaseWrite,
		WritePhaseSync,
		WritePhaseRename,
		WritePhaseFinalize,
		WritePhaseClose,
		WritePhaseRemove,
	} {
		phase := phase
		t.Run(phase.String(), func(t *testing.T) {
			err := &WriteError{
				File:  "backend",
				Phase: phase,
				Cause: errors.New("state persistence failed"),
			}
			if got := err.Code(); got != protocol.CodeStateWriteFailed {
				t.Fatalf(
					"WriteError.Code() = %q, want %q",
					got,
					protocol.CodeStateWriteFailed,
				)
			}
		})
	}
}

func TestStore_WriteUsesStableJSONAndEveryField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fileKind filesystem.StateFileKind
		write    func(*testing.T, *Store) (any, error)
	}{
		{
			name:     "transaction",
			fileKind: filesystem.StateBackend,
			write: func(t *testing.T, store *Store) (any, error) {
				t.Helper()
				value := validTransactionState(TransactionBackend)
				value.TargetVersion = ""
				return value, store.WriteTransaction(
					t.Context(),
					TransactionBackend,
					value,
				)
			},
		},
		{
			name:     "ready_environment",
			fileKind: filesystem.StateEnvironment,
			write: func(t *testing.T, store *Store) (any, error) {
				t.Helper()
				value, err := store.NewReadyEnvironment("v5.3.0", testOldCommit)
				if err != nil {
					return nil, err
				}
				return value, store.WriteEnvironment(t.Context(), value)
			},
		},
		{
			name:     "broken_environment",
			fileKind: filesystem.StateEnvironment,
			write: func(t *testing.T, store *Store) (any, error) {
				t.Helper()
				value, err := store.NewBrokenEnvironment(
					validLastSuccessful(),
					validOperationFailed(store),
				)
				if err != nil {
					return nil, err
				}
				return value, store.WriteEnvironment(t.Context(), value)
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			var gotKind filesystem.StateFileKind
			var gotPayload []byte
			files := completeFakeStateFiles()
			files.write = func(
				_ context.Context,
				kind filesystem.StateFileKind,
				payload []byte,
			) (writeResult, error) {
				gotKind = kind
				gotPayload = append([]byte(nil), payload...)
				return writeResult{mutationApplied: true}, nil
			}
			store := newTestStore(t, files, fixedStateTime)
			value, err := test.write(t, store)
			if err != nil {
				t.Fatalf("write() error = %v", err)
			}
			if gotKind != test.fileKind {
				t.Fatalf("WriteAtomic kind = %q, want %q", gotKind, test.fileKind)
			}
			wantPayload := mustStateJSON(t, value)
			if !bytes.Equal(gotPayload, wantPayload) {
				t.Fatalf("payload = %q, want %q", gotPayload, wantPayload)
			}
			if len(gotPayload) < 2 ||
				gotPayload[len(gotPayload)-1] != '\n' ||
				gotPayload[len(gotPayload)-2] != '}' {
				t.Fatalf("payload tail = %q, want object plus one LF", gotPayload)
			}
		})
	}
}

func TestStore_WriteRejectsInvalidInputBeforeIO(t *testing.T) {
	t.Parallel()

	writeCalls := 0
	files := completeFakeStateFiles()
	files.write = func(
		context.Context,
		filesystem.StateFileKind,
		[]byte,
	) (writeResult, error) {
		writeCalls++
		return writeResult{mutationApplied: true}, nil
	}
	store := newTestStore(t, files, fixedStateTime)
	validEnvironment, err := store.NewReadyEnvironment("v5.3.0", testOldCommit)
	if err != nil {
		t.Fatalf("NewReadyEnvironment() error = %v", err)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "nil_context",
			call: func() error {
				return store.WriteTransaction(
					nil,
					TransactionBackend,
					validTransactionState(TransactionBackend),
				)
			},
		},
		{
			name: "canceled_context",
			call: func() error {
				return store.WriteEnvironment(canceled, validEnvironment)
			},
		},
		{
			name: "unknown_kind",
			call: func() error {
				return store.WriteTransaction(
					t.Context(),
					TransactionKind("future"),
					validTransactionState(TransactionBackend),
				)
			},
		},
		{
			name: "invalid_transaction",
			call: func() error {
				value := validTransactionState(TransactionBackend)
				value.PID = 0
				return store.WriteTransaction(
					t.Context(),
					TransactionBackend,
					value,
				)
			},
		},
		{
			name: "invalid_environment",
			call: func() error {
				value := validEnvironment
				value.Status = protocol.StateEnvironmentBroken
				return store.WriteEnvironment(t.Context(), value)
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			before := writeCalls
			if err := test.call(); err == nil {
				t.Fatal("write error = nil, want rejection")
			}
			if writeCalls != before {
				t.Fatalf("WriteAtomic calls = %d, want %d", writeCalls, before)
			}
		})
	}
}

func TestStore_WritePreservesCommitAndRecoveryFacts(t *testing.T) {
	t.Parallel()

	primary := errors.New("primary write fault")
	cleanup := errors.New("cleanup fault")
	for _, phase := range []WritePhase{
		WritePhaseRecover,
		WritePhaseCreate,
		WritePhaseWrite,
		WritePhaseSync,
		WritePhaseRename,
		WritePhaseFinalize,
		WritePhaseClose,
	} {
		phase := phase
		t.Run(phase.String(), func(t *testing.T) {
			files := completeFakeStateFiles()
			files.write = func(
				context.Context,
				filesystem.StateFileKind,
				[]byte,
			) (writeResult, error) {
				applied := phase == WritePhaseFinalize ||
					phase == WritePhaseClose
				recovery := phase == WritePhaseRecover ||
					phase == WritePhaseRename ||
					phase == WritePhaseFinalize ||
					phase == WritePhaseClose
				return writeResult{
						mutationApplied:  applied,
						recoveryRequired: recovery,
					},
					&filesystem.StateWriteError{
						Phase:            filesystem.StateWritePhase(phase),
						MutationApplied:  applied,
						RecoveryRequired: recovery,
						Cause:            primary,
						CleanupError:     cleanup,
					}
			}
			store := newTestStore(t, files, fixedStateTime)
			err := store.WriteTransaction(
				t.Context(),
				TransactionBackend,
				validTransactionState(TransactionBackend),
			)
			var writeErr *WriteError
			if !errors.As(err, &writeErr) {
				t.Fatalf("WriteTransaction() error = %v, want *WriteError", err)
			}
			if writeErr.Phase != phase {
				t.Fatalf("WriteError.Phase = %q, want %q", writeErr.Phase, phase)
			}
			wantApplied := phase == WritePhaseFinalize ||
				phase == WritePhaseClose
			wantRecovery := phase == WritePhaseRecover ||
				phase == WritePhaseRename ||
				phase == WritePhaseFinalize ||
				phase == WritePhaseClose
			if writeErr.MutationApplied != wantApplied ||
				writeErr.RecoveryRequired != wantRecovery {
				t.Fatalf(
					"write facts = %t/%t, want %t/%t",
					writeErr.MutationApplied,
					writeErr.RecoveryRequired,
					wantApplied,
					wantRecovery,
				)
			}
			if !errors.Is(err, primary) || !errors.Is(err, cleanup) {
				t.Fatalf("WriteError chain = %v, want primary and cleanup", err)
			}
			if writeErr.Code() != protocol.CodeStateWriteFailed {
				t.Fatalf(
					"WriteError.Code() = %q, want %q",
					writeErr.Code(),
					protocol.CodeStateWriteFailed,
				)
			}
		})
	}

	t.Run("encode", func(t *testing.T) {
		files := completeFakeStateFiles()
		store := newTestStoreWithMarshal(
			t,
			files,
			func(any, string, string) ([]byte, error) {
				return nil, primary
			},
		)
		err := store.WriteTransaction(
			t.Context(),
			TransactionBackend,
			validTransactionState(TransactionBackend),
		)
		var writeErr *WriteError
		if !errors.As(err, &writeErr) ||
			writeErr.Phase != WritePhaseEncode ||
			writeErr.MutationApplied ||
			writeErr.RecoveryRequired ||
			!errors.Is(err, primary) {
			t.Fatalf("WriteTransaction() error = %v, want encode WriteError", err)
		}
	})
}

func TestStore_WriteRejectsInvalidMarshallerOutputBeforeIO(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "non_json", payload: []byte("not-json")},
		{name: "already_newline", payload: []byte("{}\n")},
		{name: "oversized", payload: bytes.Repeat([]byte{'x'}, int(maxStateFileBytes))},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			writeCalls := 0
			files := completeFakeStateFiles()
			files.write = func(
				context.Context,
				filesystem.StateFileKind,
				[]byte,
			) (writeResult, error) {
				writeCalls++
				return writeResult{mutationApplied: true}, nil
			}
			store := newTestStoreWithMarshal(
				t,
				files,
				func(any, string, string) ([]byte, error) {
					return append([]byte(nil), test.payload...), nil
				},
			)
			err := store.WriteTransaction(
				t.Context(),
				TransactionBackend,
				validTransactionState(TransactionBackend),
			)
			var writeErr *WriteError
			if !errors.As(err, &writeErr) ||
				writeErr.Phase != WritePhaseEncode {
				t.Fatalf("WriteTransaction() error = %v, want encode WriteError", err)
			}
			if writeCalls != 0 {
				t.Fatalf("WriteAtomic calls = %d, want 0", writeCalls)
			}
		})
	}
}

func TestStore_WriteCancellationAfterCommitKeepsAppliedResult(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	files := completeFakeStateFiles()
	files.write = func(
		context.Context,
		filesystem.StateFileKind,
		[]byte,
	) (writeResult, error) {
		cancel()
		return writeResult{mutationApplied: true}, nil
	}
	store := newTestStore(t, files, fixedStateTime)
	err := store.WriteTransaction(
		ctx,
		TransactionBackend,
		validTransactionState(TransactionBackend),
	)
	if err != nil {
		t.Fatalf("WriteTransaction() error = %v, want committed success", err)
	}
}

type controlledContext struct {
	context.Context
	mu   sync.Mutex
	done chan struct{}
	err  error
}

func newControlledContext(parent context.Context) *controlledContext {
	return &controlledContext{Context: parent, done: make(chan struct{})}
}

func (c *controlledContext) Done() <-chan struct{} {
	return c.done
}

func (c *controlledContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *controlledContext) fail(err error) {
	c.mu.Lock()
	c.err = err
	close(c.done)
	c.mu.Unlock()
}

func TestStore_WritePassesThroughContextBetweenChecks(t *testing.T) {
	t.Parallel()

	for _, contextErr := range []error{
		context.Canceled,
		context.DeadlineExceeded,
	} {
		contextErr := contextErr
		t.Run(contextErr.Error(), func(t *testing.T) {
			ctx := newControlledContext(t.Context())
			writeEntered := make(chan struct{})
			releaseWrite := make(chan struct{})
			writeCalls := 0
			files := completeFakeStateFiles()
			files.write = func(
				context.Context,
				filesystem.StateFileKind,
				[]byte,
			) (writeResult, error) {
				writeCalls++
				close(writeEntered)
				<-releaseWrite
				return normalizeFilesystemWriteResult(
					filesystem.WriteAtomicResult{},
					ctx.Err(),
				)
			}
			store := newTestStore(t, files, fixedStateTime)
			result := make(chan error, 1)
			go func() {
				result <- store.WriteTransaction(
					ctx,
					TransactionBackend,
					validTransactionState(TransactionBackend),
				)
			}()
			waitForTestSignal(t, writeEntered, "StateFiles context precheck")
			ctx.fail(contextErr)
			close(releaseWrite)
			err := receiveTestValue(t, result, "Store write result")
			if err != contextErr {
				t.Fatalf("WriteTransaction() error = %v, want original %v", err, contextErr)
			}
			var writeErr *WriteError
			if errors.As(err, &writeErr) {
				t.Fatalf("WriteTransaction() error = %v, do not want *WriteError", err)
			}
			if writeCalls != 1 {
				t.Fatalf("WriteAtomic calls = %d, want 1", writeCalls)
			}
		})
	}

	for _, result := range []filesystem.WriteAtomicResult{
		{MutationApplied: true},
		{RecoveryRequired: true},
	} {
		result := result
		t.Run(fmt.Sprintf(
			"untyped_context_fails_closed_%t_%t",
			result.MutationApplied,
			result.RecoveryRequired,
		), func(t *testing.T) {
			files := completeFakeStateFiles()
			files.write = func(
				context.Context,
				filesystem.StateFileKind,
				[]byte,
			) (writeResult, error) {
				return normalizeFilesystemWriteResult(result, context.Canceled)
			}
			store := newTestStore(t, files, fixedStateTime)
			err := store.WriteTransaction(
				t.Context(),
				TransactionBackend,
				validTransactionState(TransactionBackend),
			)
			var writeErr *WriteError
			if !errors.As(err, &writeErr) ||
				writeErr.MutationApplied != result.MutationApplied ||
				writeErr.RecoveryRequired != result.RecoveryRequired ||
				writeErr.Phase != WritePhaseWrite ||
				!errors.Is(err, context.Canceled) {
				t.Fatalf("WriteTransaction() error = %#v, want fail-closed facts", writeErr)
			}
		})
	}
}

func TestStore_TypedFilesystemContextCauseRemainsWriteError(t *testing.T) {
	t.Parallel()

	for _, contextErr := range []error{
		context.Canceled,
		context.DeadlineExceeded,
	} {
		contextErr := contextErr
		t.Run(contextErr.Error(), func(t *testing.T) {
			cleanup := errors.New("cleanup after context failure")
			source := &filesystem.StateWriteError{
				Phase:        filesystem.StateWritePhaseSync,
				Cause:        fmt.Errorf("sync state: %w", contextErr),
				CleanupError: cleanup,
			}
			files := completeFakeStateFiles()
			files.write = func(
				context.Context,
				filesystem.StateFileKind,
				[]byte,
			) (writeResult, error) {
				return normalizeFilesystemWriteResult(
					filesystem.WriteAtomicResult{},
					source,
				)
			}
			store := newTestStore(t, files, fixedStateTime)
			err := store.WriteTransaction(
				t.Context(),
				TransactionBackend,
				validTransactionState(TransactionBackend),
			)
			var writeErr *WriteError
			var filesystemWriteErr *filesystem.StateWriteError
			if !errors.As(err, &writeErr) ||
				writeErr.Phase != WritePhaseSync ||
				writeErr.MutationApplied ||
				writeErr.RecoveryRequired ||
				writeErr.Code() != protocol.CodeStateWriteFailed ||
				!errors.Is(err, contextErr) ||
				!errors.Is(err, cleanup) ||
				errors.As(err, &filesystemWriteErr) {
				t.Fatalf("WriteTransaction() error = %#v, want mapped sync WriteError", writeErr)
			}
		})
	}
}

func TestStore_PostCommitFailureReportsAppliedAndRecovery(t *testing.T) {
	t.Parallel()

	for _, phase := range []filesystem.StateWritePhase{
		filesystem.StateWritePhaseFinalize,
		filesystem.StateWritePhaseClose,
	} {
		phase := phase
		t.Run(phase.String(), func(t *testing.T) {
			cause := errors.New("post-commit finalization failed")
			files := completeFakeStateFiles()
			files.write = func(
				context.Context,
				filesystem.StateFileKind,
				[]byte,
			) (writeResult, error) {
				return writeResult{
						mutationApplied:  true,
						recoveryRequired: true,
					},
					&filesystem.StateWriteError{
						Phase:            phase,
						MutationApplied:  true,
						RecoveryRequired: true,
						Cause:            cause,
					}
			}
			store := newTestStore(t, files, fixedStateTime)
			err := store.WriteEnvironment(
				t.Context(),
				EnvironmentState{
					SchemaVersion:  SchemaVersion,
					Status:         protocol.StateReadyToStart,
					UpdatedAt:      fixedStateTime().UTC(),
					LastSuccessful: validLastSuccessful(),
					Broken:         nil,
				},
			)
			var writeErr *WriteError
			if !errors.As(err, &writeErr) ||
				!writeErr.MutationApplied ||
				!writeErr.RecoveryRequired ||
				!errors.Is(err, cause) {
				t.Fatalf("WriteEnvironment() error = %#v, want true/true WriteError", writeErr)
			}
		})
	}
}

func TestStore_ReadRejectsUnsealedOrInconsistentIntent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want error
	}{
		{
			name: "truncated_or_unsealed_intent",
			err:  filesystem.ErrStateRecoveryRequired,
			want: filesystem.ErrStateRecoveryRequired,
		},
		{
			name: "intent_identity_or_envelope_mismatch",
			err:  filesystem.ErrIdentityChanged,
			want: filesystem.ErrIdentityChanged,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			files := completeFakeStateFiles()
			files.read = func(
				context.Context,
				filesystem.StateFileKind,
				int64,
			) (stateFileSnapshot, error) {
				return nil, test.err
			}
			store := newTestStore(t, files, fixedStateTime)
			_, err := store.ReadTransaction(t.Context(), TransactionMutation)
			var readErr *ReadError
			if !errors.As(err, &readErr) ||
				!errors.Is(err, test.want) ||
				errors.Is(err, ErrNotFound) ||
				errors.Is(err, ErrCorrupt) {
				t.Fatalf("ReadTransaction() error = %v, want recovery/identity ReadError", err)
			}
		})
	}
}

func TestStore_RemoveTransactionResultMatrix(t *testing.T) {
	t.Parallel()

	closeCause := errors.New("close state guard")
	tests := []struct {
		name         string
		result       removeResult
		err          error
		wantNil      bool
		wantChanged  bool
		wantWrite    bool
		wantApplied  bool
		wantRecovery bool
		wantCleanup  bool
	}{
		{name: "stable_missing", wantNil: true},
		{
			name:        "clean_mismatch",
			err:         filesystem.ErrIdentityChanged,
			wantChanged: true,
		},
		{
			name: "foreign_intent",
			result: removeResult{
				recoveryRequired: true,
			},
			err:          filesystem.ErrStateRecoveryRequired,
			wantWrite:    true,
			wantRecovery: true,
		},
		{
			name: "stable_missing_guard_close",
			result: removeResult{
				recoveryRequired: true,
			},
			err:          closeCause,
			wantWrite:    true,
			wantRecovery: true,
		},
		{
			name: "mismatch_guard_close",
			result: removeResult{
				recoveryRequired: true,
			},
			err: &filesystem.StateRemoveError{
				Cause:        filesystem.ErrIdentityChanged,
				CleanupError: closeCause,
			},
			wantChanged:  true,
			wantWrite:    true,
			wantRecovery: true,
			wantCleanup:  true,
		},
		{
			name: "unlink_success",
			result: removeResult{
				mutationApplied: true,
			},
			wantNil: true,
		},
		{
			name: "post_unlink_close",
			result: removeResult{
				mutationApplied:  true,
				recoveryRequired: true,
			},
			err: &filesystem.StateRemoveError{
				Cause:        errors.New("post-unlink verification"),
				CleanupError: closeCause,
			},
			wantWrite:    true,
			wantApplied:  true,
			wantRecovery: true,
			wantCleanup:  true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			files := completeFakeStateFiles()
			store := newTestStore(t, files, fixedStateTime)
			snapshot := readTestTransactionSnapshot(
				t,
				store,
				files,
				TransactionMutation,
			)
			files.remove = func(
				context.Context,
				stateFileSnapshot,
			) (removeResult, error) {
				return test.result, test.err
			}
			err := store.RemoveTransaction(t.Context(), snapshot)
			if (err == nil) != test.wantNil {
				t.Fatalf("RemoveTransaction() error = %v, wantNil %t", err, test.wantNil)
			}
			if errors.Is(err, ErrTransactionChanged) != test.wantChanged {
				t.Fatalf("changed classification = %t, want %t", errors.Is(err, ErrTransactionChanged), test.wantChanged)
			}
			var writeErr *WriteError
			if errors.As(err, &writeErr) != test.wantWrite {
				t.Fatalf("WriteError classification = %t, want %t", errors.As(err, &writeErr), test.wantWrite)
			}
			if writeErr != nil {
				if writeErr.Phase != WritePhaseRemove ||
					writeErr.MutationApplied != test.wantApplied ||
					writeErr.RecoveryRequired != test.wantRecovery {
					t.Fatalf("WriteError = %#v, want remove %t/%t", writeErr, test.wantApplied, test.wantRecovery)
				}
				if (writeErr.CleanupError != nil) != test.wantCleanup {
					t.Fatalf("CleanupError = %v, want present %t", writeErr.CleanupError, test.wantCleanup)
				}
			}
			if test.wantCleanup && !errors.Is(err, closeCause) {
				t.Fatalf("RemoveTransaction() error = %v, want close chain", err)
			}
		})
	}
}

func TestStore_RemoveRecoveryRequiredFailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		result       removeResult
		err          error
		wantWrite    bool
		wantRecovery bool
	}{
		{
			name: "active_gate_cancel",
			err:  context.Canceled,
		},
		{
			name: "foreign_fixed_intent",
			result: removeResult{
				recoveryRequired: true,
			},
			err:          filesystem.ErrStateRecoveryRequired,
			wantWrite:    true,
			wantRecovery: true,
		},
		{
			name: "foreign_destination",
			result: removeResult{
				recoveryRequired: true,
			},
			err:          filesystem.ErrIdentityChanged,
			wantWrite:    true,
			wantRecovery: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			files := completeFakeStateFiles()
			store := newTestStore(t, files, fixedStateTime)
			snapshot := readTestTransactionSnapshot(
				t,
				store,
				files,
				TransactionUpdate,
			)
			files.remove = func(
				context.Context,
				stateFileSnapshot,
			) (removeResult, error) {
				return test.result, test.err
			}
			err := store.RemoveTransaction(t.Context(), snapshot)
			var writeErr *WriteError
			if errors.As(err, &writeErr) != test.wantWrite {
				t.Fatalf("WriteError classification = %t, want %t", errors.As(err, &writeErr), test.wantWrite)
			}
			if test.wantWrite &&
				(writeErr.MutationApplied || writeErr.RecoveryRequired != test.wantRecovery) {
				t.Fatalf("WriteError facts = %t/%t, want false/%t", writeErr.MutationApplied, writeErr.RecoveryRequired, test.wantRecovery)
			}
			if !test.wantWrite && err != context.Canceled {
				t.Fatalf("RemoveTransaction() error = %v, want original context.Canceled", err)
			}
		})
	}
}

func TestStore_RemoveTransactionRejectsChangedSnapshot(t *testing.T) {
	t.Parallel()

	files := completeFakeStateFiles()
	store := newTestStore(t, files, fixedStateTime)
	snapshot := readTestTransactionSnapshot(
		t,
		store,
		files,
		TransactionBackend,
	)
	files.remove = func(
		context.Context,
		stateFileSnapshot,
	) (removeResult, error) {
		return removeResult{}, filesystem.ErrIdentityChanged
	}
	err := store.RemoveTransaction(t.Context(), snapshot)
	if !errors.Is(err, ErrTransactionChanged) ||
		!errors.Is(err, filesystem.ErrIdentityChanged) {
		t.Fatalf("RemoveTransaction() error = %v, want changed error chain", err)
	}
	var writeErr *WriteError
	if errors.As(err, &writeErr) {
		t.Fatalf("RemoveTransaction() error = %v, do not want WriteError", err)
	}
}

func TestTransactionSnapshot_CannotAuthorizeAnotherStore(t *testing.T) {
	t.Parallel()

	removeCalls := 0
	filesOne := completeFakeStateFiles()
	filesOne.remove = func(
		context.Context,
		stateFileSnapshot,
	) (removeResult, error) {
		removeCalls++
		return removeResult{mutationApplied: true}, nil
	}
	storeOne := newTestStore(t, filesOne, fixedStateTime)
	snapshot := readTestTransactionSnapshot(
		t,
		storeOne,
		filesOne,
		TransactionMutation,
	)

	filesTwo := completeFakeStateFiles()
	filesTwo.remove = filesOne.remove
	storeTwo := newTestStore(t, filesTwo, fixedStateTime)
	if err := storeTwo.RemoveTransaction(t.Context(), snapshot); err == nil {
		t.Fatal("cross-store RemoveTransaction() error = nil, want rejection")
	}
	if err := storeOne.RemoveTransaction(
		t.Context(),
		TransactionSnapshot{},
	); err == nil {
		t.Fatal("zero snapshot RemoveTransaction() error = nil, want rejection")
	}
	if removeCalls != 0 {
		t.Fatalf("RemoveTransactionIfUnchanged calls = %d, want 0", removeCalls)
	}

	stateCopy := snapshot.State()
	stateCopy.OperationID = "01BX5ZZKBKACTAV9WEVGEMMVRZ"
	if err := storeOne.RemoveTransaction(t.Context(), snapshot); err != nil {
		t.Fatalf("original snapshot RemoveTransaction() error = %v", err)
	}
	if removeCalls != 1 {
		t.Fatalf("RemoveTransactionIfUnchanged calls = %d, want 1", removeCalls)
	}
}

func TestStore_CloseIsIdempotent(t *testing.T) {
	t.Parallel()

	closeCause := errors.New("close state root")
	closeCalls := 0
	files := completeFakeStateFiles()
	files.close = func() error {
		closeCalls++
		return closeCause
	}
	store, err := newStoreWithDependencies(
		t.Context(),
		mustTestLayout(t),
		storeDependencies{
			openFiles: func(context.Context, *config.Layout) (stateFiles, error) {
				return files, nil
			},
			marshalIndent: json.MarshalIndent,
		},
		WithClock(fixedStateTime),
	)
	if err != nil {
		t.Fatalf("newStoreWithDependencies() error = %v", err)
	}
	ready, err := store.NewReadyEnvironment("v5.3.0", testOldCommit)
	if err != nil {
		t.Fatalf("NewReadyEnvironment() error = %v", err)
	}
	if err := store.Close(); !errors.Is(err, closeCause) {
		t.Fatalf("first Close() error = %v, want close cause", err)
	}
	if err := store.Close(); !errors.Is(err, closeCause) {
		t.Fatalf("second Close() error = %v, want cached close cause", err)
	}
	if closeCalls != 1 {
		t.Fatalf("StateFiles.Close calls = %d, want 1", closeCalls)
	}

	ioCalls := []func() error{
		func() error {
			_, err := store.ReadTransaction(
				t.Context(),
				TransactionKind("future"),
			)
			return err
		},
		func() error {
			return store.WriteTransaction(
				t.Context(),
				TransactionBackend,
				validTransactionState(TransactionBackend),
			)
		},
		func() error {
			return store.RemoveTransaction(t.Context(), TransactionSnapshot{})
		},
		func() error {
			_, err := store.ReadEnvironment(t.Context())
			return err
		},
		func() error {
			return store.WriteEnvironment(t.Context(), ready)
		},
	}
	for index, call := range ioCalls {
		if err := call(); !errors.Is(err, ErrClosed) {
			t.Fatalf("closed I/O %d error = %v, want ErrClosed", index, err)
		}
	}

	transaction, err := store.NewTransaction(
		TransactionBackend,
		validTransactionInput(TransactionBackend),
	)
	if err != nil || !reflect.DeepEqual(
		transaction,
		validTransactionState(TransactionBackend),
	) {
		t.Fatalf("NewTransaction() after Close = %#v, %v", transaction, err)
	}
	if err := store.ValidateEnvironment(ready); err != nil {
		t.Fatalf("ValidateEnvironment() after Close error = %v", err)
	}
}
