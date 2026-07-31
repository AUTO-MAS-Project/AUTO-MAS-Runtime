package state

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

var errFakeDependencyMissing = errors.New("fake state dependency is missing")

const testBarrierTimeout = 5 * time.Second

type fakeStateFileSnapshot struct {
	fileKind filesystem.StateFileKind
	payload  []byte
}

func (s fakeStateFileSnapshot) kind() filesystem.StateFileKind {
	return s.fileKind
}

func (s fakeStateFileSnapshot) bytes() []byte {
	return append([]byte(nil), s.payload...)
}

func (fakeStateFileSnapshot) stateFileSnapshot() {}

type fakeStateFiles struct {
	read   func(context.Context, filesystem.StateFileKind, int64) (stateFileSnapshot, error)
	write  func(context.Context, filesystem.StateFileKind, []byte) (writeResult, error)
	remove func(context.Context, stateFileSnapshot) (removeResult, error)
	close  func() error
}

func (f *fakeStateFiles) Read(
	ctx context.Context,
	kind filesystem.StateFileKind,
	maxBytes int64,
) (stateFileSnapshot, error) {
	if f.read == nil {
		return nil, errFakeDependencyMissing
	}
	return f.read(ctx, kind, maxBytes)
}

func (f *fakeStateFiles) WriteAtomic(
	ctx context.Context,
	kind filesystem.StateFileKind,
	payload []byte,
) (writeResult, error) {
	if f.write == nil {
		return writeResult{}, errFakeDependencyMissing
	}
	return f.write(ctx, kind, append([]byte(nil), payload...))
}

func (f *fakeStateFiles) RemoveTransactionIfUnchanged(
	ctx context.Context,
	snapshot stateFileSnapshot,
) (removeResult, error) {
	if f.remove == nil {
		return removeResult{}, errFakeDependencyMissing
	}
	return f.remove(ctx, snapshot)
}

func (f *fakeStateFiles) Close() error {
	if f.close == nil {
		return errFakeDependencyMissing
	}
	return f.close()
}

func completeFakeStateFiles() *fakeStateFiles {
	return &fakeStateFiles{
		read: func(
			context.Context,
			filesystem.StateFileKind,
			int64,
		) (stateFileSnapshot, error) {
			return nil, errFakeDependencyMissing
		},
		write: func(
			context.Context,
			filesystem.StateFileKind,
			[]byte,
		) (writeResult, error) {
			return writeResult{mutationApplied: true}, nil
		},
		remove: func(
			context.Context,
			stateFileSnapshot,
		) (removeResult, error) {
			return removeResult{mutationApplied: true}, nil
		},
		close: func() error { return nil },
	}
}

func mustTestLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	return layout
}

func newTestStore(
	t *testing.T,
	files stateFiles,
	clock func() time.Time,
) *Store {
	t.Helper()
	store, err := newStoreWithDependencies(
		t.Context(),
		mustTestLayout(t),
		storeDependencies{
			openFiles: func(context.Context, *config.Layout) (stateFiles, error) {
				return files, nil
			},
			marshalIndent: json.MarshalIndent,
		},
		WithClock(clock),
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

func fixedStateTime() time.Time {
	return time.Date(2026, 7, 29, 9, 10, 11, 123456700, time.FixedZone("CST", 8*60*60))
}

func receiveTestValue[T any](
	t *testing.T,
	values <-chan T,
	name string,
) T {
	t.Helper()
	timer := time.NewTimer(testBarrierTimeout)
	defer timer.Stop()
	select {
	case value := <-values:
		return value
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", name)
		var zero T
		return zero
	}
}

func waitForTestSignal(
	t *testing.T,
	signal <-chan struct{},
	name string,
) {
	t.Helper()
	receiveTestValue(t, signal, name)
}

func validTransactionInput(kind TransactionKind) TransactionInput {
	input := TransactionInput{
		OperationID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		PID:         12345,
	}
	switch kind {
	case TransactionBackend:
		input.Command = "backend supervise"
		input.Stage = protocol.StageBackendSpawn
	case TransactionMutation:
		input.Command = "workspace sync"
		input.TargetVersion = "v5.4.0-beta.1"
		input.Stage = protocol.StageWorkspaceCheck
	case TransactionUpdate:
		input.Command = "workspace sync"
		input.TargetVersion = "v5.4.0-beta.1"
		input.Stage = protocol.StageWorkspaceClone
	default:
		panic("invalid test transaction kind")
	}
	return input
}

func validTransactionState(kind TransactionKind) TransactionState {
	input := validTransactionInput(kind)
	return TransactionState{
		SchemaVersion: SchemaVersion,
		OperationID:   input.OperationID,
		Command:       input.Command,
		PID:           input.PID,
		StartedAt:     fixedStateTime().UTC(),
		TargetVersion: input.TargetVersion,
		Stage:         input.Stage,
	}
}
