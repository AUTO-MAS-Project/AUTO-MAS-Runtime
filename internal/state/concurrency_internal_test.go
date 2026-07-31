package state

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

func TestStore_SingleMutexLinearizesClockIOAndClose(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{
		"clock",
		"read",
		"write",
		"remove",
		"close",
	} {
		operation := operation
		t.Run(operation, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			files := completeFakeStateFiles()
			clock := fixedStateTime

			switch operation {
			case "clock":
				clock = func() time.Time {
					close(entered)
					<-release
					return fixedStateTime()
				}
			case "read":
				readPayload := mustStateJSON(
					t,
					validTransactionState(TransactionBackend),
				)
				files.read = func(
					context.Context,
					filesystem.StateFileKind,
					int64,
				) (stateFileSnapshot, error) {
					close(entered)
					<-release
					return fakeSnapshot(
						filesystem.StateBackend,
						readPayload,
					), nil
				}
			case "write":
				files.write = func(
					context.Context,
					filesystem.StateFileKind,
					[]byte,
				) (writeResult, error) {
					close(entered)
					<-release
					return writeResult{mutationApplied: true}, nil
				}
			case "close":
				files.close = func() error {
					close(entered)
					<-release
					return nil
				}
			}

			store := newTestStore(t, files, clock)
			var snapshot TransactionSnapshot
			if operation == "remove" {
				snapshot = readTestTransactionSnapshot(
					t,
					store,
					files,
					TransactionMutation,
				)
				files.remove = func(
					context.Context,
					stateFileSnapshot,
				) (removeResult, error) {
					close(entered)
					<-release
					return removeResult{mutationApplied: true}, nil
				}
			}

			done := make(chan error, 1)
			go func() {
				switch operation {
				case "clock":
					_, err := store.NewTransaction(
						TransactionBackend,
						validTransactionInput(TransactionBackend),
					)
					done <- err
				case "read":
					_, err := store.ReadTransaction(
						t.Context(),
						TransactionBackend,
					)
					done <- err
				case "write":
					done <- store.WriteTransaction(
						t.Context(),
						TransactionBackend,
						validTransactionState(TransactionBackend),
					)
				case "remove":
					done <- store.RemoveTransaction(t.Context(), snapshot)
				case "close":
					done <- store.Close()
				default:
					done <- fmt.Errorf("unknown operation %s", operation)
				}
			}()

			waitForTestSignal(t, entered, operation+" dependency")
			if store.mu.TryLock() {
				store.mu.Unlock()
				close(release)
				receiveTestValue(t, done, operation+" failure result")
				t.Fatalf("%s dependency ran without Store mutex", operation)
			}
			close(release)
			if err := receiveTestValue(
				t,
				done,
				operation+" result",
			); err != nil {
				t.Fatalf("%s error = %v", operation, err)
			}
		})
	}
}

func TestStore_ConcurrentWritesAreLogicalWholeValues(t *testing.T) {
	t.Parallel()

	const writers = 32
	var payloadMu sync.Mutex
	payloads := make([][]byte, 0, writers)
	files := completeFakeStateFiles()
	files.write = func(
		_ context.Context,
		kind filesystem.StateFileKind,
		payload []byte,
	) (writeResult, error) {
		if kind != filesystem.StateMutation {
			return writeResult{}, fmt.Errorf("kind %s is not mutation", kind)
		}
		if _, err := decodeTransaction(
			"mutation",
			TransactionMutation,
			payload,
		); err != nil {
			return writeResult{}, fmt.Errorf("decode complete payload: %w", err)
		}
		payloadMu.Lock()
		payloads = append(payloads, append([]byte(nil), payload...))
		payloadMu.Unlock()
		return writeResult{mutationApplied: true}, nil
	}
	store := newTestStore(t, files, fixedStateTime)

	var wait sync.WaitGroup
	errs := make(chan error, writers)
	for index := 0; index < writers; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			value := validTransactionState(TransactionMutation)
			value.TargetVersion = fmt.Sprintf("v5.4.0-writer-%d", index)
			errs <- store.WriteTransaction(
				t.Context(),
				TransactionMutation,
				value,
			)
		}()
	}
	allDone := make(chan struct{})
	go func() {
		wait.Wait()
		close(allDone)
	}()
	waitForTestSignal(t, allDone, "concurrent writers")
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent WriteTransaction() error = %v", err)
		}
	}

	payloadMu.Lock()
	defer payloadMu.Unlock()
	if len(payloads) != writers {
		t.Fatalf("payload count = %d, want %d", len(payloads), writers)
	}
	for index, payload := range payloads {
		if _, err := decodeTransaction(
			"mutation",
			TransactionMutation,
			payload,
		); err != nil {
			t.Fatalf("payload %d decode error = %v", index, err)
		}
	}
}

func TestStore_ReadIntentGapIsNotNotFound(t *testing.T) {
	t.Parallel()

	for _, want := range []TransactionState{
		validTransactionState(TransactionMutation),
		func() TransactionState {
			value := validTransactionState(TransactionMutation)
			value.TargetVersion = "v5.4.0-new"
			return value
		}(),
	} {
		want := want
		t.Run(want.TargetVersion, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			files := completeFakeStateFiles()
			files.read = func(
				context.Context,
				filesystem.StateFileKind,
				int64,
			) (stateFileSnapshot, error) {
				close(entered)
				<-release
				return fakeSnapshot(
					filesystem.StateMutation,
					mustStateJSON(t, want),
				), nil
			}
			store := newTestStore(t, files, fixedStateTime)
			result := make(chan error, 1)
			go func() {
				snapshot, err := store.ReadTransaction(
					t.Context(),
					TransactionMutation,
				)
				if err == nil && snapshot.State() != want {
					err = fmt.Errorf("State() = %#v, want %#v", snapshot.State(), want)
				}
				result <- err
			}()
			waitForTestSignal(t, entered, "shared gate wait")
			close(release)
			if err := receiveTestValue(t, result, "logical state read"); err != nil {
				t.Fatalf("ReadTransaction() error = %v", err)
			}
		})
	}

	t.Run("wait_context", func(t *testing.T) {
		entered := make(chan struct{})
		files := completeFakeStateFiles()
		files.read = func(
			ctx context.Context,
			_ filesystem.StateFileKind,
			_ int64,
		) (stateFileSnapshot, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		store := newTestStore(t, files, fixedStateTime)
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() {
			_, err := store.ReadTransaction(ctx, TransactionMutation)
			result <- err
		}()
		waitForTestSignal(t, entered, "shared gate cancellation")
		cancel()
		err := receiveTestValue(t, result, "canceled state read")
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNotFound) {
			t.Fatalf("ReadTransaction() error = %v, want context without ErrNotFound", err)
		}
	})
}

func TestStore_ReadForeignDestinationUsesProvenBackup(t *testing.T) {
	t.Parallel()

	old := validTransactionState(TransactionUpdate)
	files := completeFakeStateFiles()
	files.read = func(
		context.Context,
		filesystem.StateFileKind,
		int64,
	) (stateFileSnapshot, error) {
		// T2.5 已证明 D=X/B=O/T=N，adapter 只接收权威 B=O snapshot。
		return fakeSnapshot(
			filesystem.StateUpdate,
			mustStateJSON(t, old),
		), nil
	}
	store := newTestStore(t, files, fixedStateTime)
	snapshot, err := store.ReadTransaction(t.Context(), TransactionUpdate)
	if err != nil || snapshot.State() != old {
		t.Fatalf("ReadTransaction() = %#v, %v, want proven old", snapshot.State(), err)
	}

	for _, test := range []struct {
		name  string
		cause error
	}{
		{name: "backup_missing", cause: filesystem.ErrStateRecoveryRequired},
		{name: "backup_replaced", cause: filesystem.ErrIdentityChanged},
		{name: "intent_replaced", cause: filesystem.ErrIdentityChanged},
		{name: "destination_opaque", cause: filesystem.ErrIdentityChanged},
		{name: "destination_unopenable", cause: filesystem.ErrIdentityChanged},
		{name: "destination_reparse", cause: filesystem.ErrIdentityChanged},
		{name: "destination_hardlink", cause: filesystem.ErrIdentityChanged},
		{name: "destination_identity_unprovable", cause: filesystem.ErrIdentityChanged},
		{name: "ordinary_proof_degraded", cause: filesystem.ErrStateRecoveryRequired},
		{name: "size_proof_degraded", cause: filesystem.ErrStateRecoveryRequired},
		{name: "digest_proof_degraded", cause: filesystem.ErrStateRecoveryRequired},
		{name: "root_proof_degraded", cause: filesystem.ErrStateRecoveryRequired},
		{name: "kind_proof_degraded", cause: filesystem.ErrStateRecoveryRequired},
		{name: "leaf_proof_degraded", cause: filesystem.ErrStateRecoveryRequired},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			files.read = func(
				context.Context,
				filesystem.StateFileKind,
				int64,
			) (stateFileSnapshot, error) {
				return nil, test.cause
			}
			_, err := store.ReadTransaction(t.Context(), TransactionUpdate)
			var readErr *ReadError
			if !errors.As(err, &readErr) ||
				!errors.Is(err, test.cause) ||
				errors.Is(err, ErrNotFound) {
				t.Fatalf("ReadTransaction() error = %v, want recovery/identity ReadError", err)
			}
		})
	}
}

func TestStore_ClockIsInjected(t *testing.T) {
	t.Parallel()

	now := fixedStateTime()
	calls := 0
	store := newTestStore(t, completeFakeStateFiles(), func() time.Time {
		calls++
		return now
	})
	transaction, err := store.NewTransaction(
		TransactionBackend,
		validTransactionInput(TransactionBackend),
	)
	if err != nil {
		t.Fatalf("NewTransaction() error = %v", err)
	}
	environment, err := store.NewReadyEnvironment("v5.3.0", testOldCommit)
	if err != nil {
		t.Fatalf("NewReadyEnvironment() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("clock calls = %d, want 2", calls)
	}
	if !transaction.StartedAt.Equal(now) ||
		transaction.StartedAt.Location() != time.UTC {
		t.Fatalf("StartedAt = %v, want %v UTC", transaction.StartedAt, now)
	}
	if !environment.UpdatedAt.Equal(now) ||
		environment.UpdatedAt.Location() != time.UTC {
		t.Fatalf("UpdatedAt = %v, want %v UTC", environment.UpdatedAt, now)
	}
}
