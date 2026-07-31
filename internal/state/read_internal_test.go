package state

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"syscall"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func fakeSnapshot(
	kind filesystem.StateFileKind,
	payload []byte,
) stateFileSnapshot {
	return fakeStateFileSnapshot{
		fileKind: kind,
		payload:  append([]byte(nil), payload...),
	}
}

func TestStore_TransactionRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		kind     TransactionKind
		fileKind filesystem.StateFileKind
	}{
		{name: "backend", kind: TransactionBackend, fileKind: filesystem.StateBackend},
		{name: "mutation", kind: TransactionMutation, fileKind: filesystem.StateMutation},
		{name: "update", kind: TransactionUpdate, fileKind: filesystem.StateUpdate},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			value := validTransactionState(test.kind)
			files := completeFakeStateFiles()
			files.read = func(
				_ context.Context,
				gotKind filesystem.StateFileKind,
				maxBytes int64,
			) (stateFileSnapshot, error) {
				if gotKind != test.fileKind {
					t.Fatalf("Read kind = %q, want %q", gotKind, test.fileKind)
				}
				if maxBytes != filesystem.MaxStateFileBytes {
					t.Fatalf(
						"Read maxBytes = %d, want %d",
						maxBytes,
						filesystem.MaxStateFileBytes,
					)
				}
				return fakeSnapshot(test.fileKind, mustStateJSON(t, value)), nil
			}
			store := newTestStore(t, files, fixedStateTime)
			snapshot, err := store.ReadTransaction(t.Context(), test.kind)
			if err != nil {
				t.Fatalf("ReadTransaction() error = %v", err)
			}
			if snapshot.Kind() != test.kind {
				t.Fatalf("snapshot.Kind() = %q, want %q", snapshot.Kind(), test.kind)
			}
			if got := snapshot.State(); !reflect.DeepEqual(got, value) {
				t.Fatalf("snapshot.State() = %#v, want %#v", got, value)
			}
		})
	}
}

func TestStore_EnvironmentRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		build func(*testing.T, *Store) EnvironmentState
	}{
		{
			name: "ready",
			build: func(t *testing.T, store *Store) EnvironmentState {
				t.Helper()
				value, err := store.NewReadyEnvironment("v5.3.0", testOldCommit)
				if err != nil {
					t.Fatalf("NewReadyEnvironment() error = %v", err)
				}
				return value
			},
		},
		{
			name: "repository_changed",
			build: func(t *testing.T, store *Store) EnvironmentState {
				t.Helper()
				value, err := store.NewBrokenEnvironment(
					validLastSuccessful(),
					validRepositoryChanged(store),
				)
				if err != nil {
					t.Fatalf("NewBrokenEnvironment() error = %v", err)
				}
				return value
			},
		},
		{
			name: "operation_failed",
			build: func(t *testing.T, store *Store) EnvironmentState {
				t.Helper()
				value, err := store.NewBrokenEnvironment(
					validLastSuccessful(),
					validOperationFailed(store),
				)
				if err != nil {
					t.Fatalf("NewBrokenEnvironment() error = %v", err)
				}
				return value
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			files := completeFakeStateFiles()
			store := newTestStore(t, files, fixedStateTime)
			want := test.build(t, store)
			files.read = func(
				context.Context,
				filesystem.StateFileKind,
				int64,
			) (stateFileSnapshot, error) {
				return fakeSnapshot(
					filesystem.StateEnvironment,
					mustStateJSON(t, want),
				), nil
			}
			got, err := store.ReadEnvironment(t.Context())
			if err != nil {
				t.Fatalf("ReadEnvironment() error = %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ReadEnvironment() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestStore_ReadDistinguishesMissingCorruptAndUnsupportedSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		readErr     error
		payload     []byte
		want        error
		wantReadErr bool
	}{
		{
			name:    "stable_missing_with_unreferenced_orphan_t_j",
			readErr: filesystem.ErrStateFileNotFound,
			want:    ErrNotFound,
		},
		{
			name:        "raw_fs_not_exist",
			readErr:     fs.ErrNotExist,
			want:        fs.ErrNotExist,
			wantReadErr: true,
		},
		{
			name: "win32_name_not_found",
			readErr: &filesystem.FileError{
				Operation: "open-state-leaf",
				Path:      `D:\managed\runtime-state\backend.json`,
				Err:       syscall.Errno(2),
			},
			want:        syscall.Errno(2),
			wantReadErr: true,
		},
		{
			name: "ordinary_file_error",
			readErr: &filesystem.FileError{
				Operation: "read-state-leaf",
				Path:      `D:\managed\runtime-state\backend.json`,
				Err:       fs.ErrPermission,
			},
			want:        fs.ErrPermission,
			wantReadErr: true,
		},
		{name: "malformed", payload: []byte("{\n"), want: ErrCorrupt},
		{
			name:    "unknown_field",
			payload: []byte(`{"schemaVersion":1,"future":true}` + "\n"),
			want:    ErrCorrupt,
		},
		{
			name:    "duplicate",
			payload: []byte(`{"schemaVersion":1,"pid":1,"pid":2}` + "\n"),
			want:    ErrCorrupt,
		},
		{
			name:    "unsupported",
			payload: []byte(`{"schemaVersion":2}` + "\n"),
			want:    ErrUnsupportedSchema,
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
				if test.readErr != nil {
					return nil, test.readErr
				}
				return fakeSnapshot(filesystem.StateBackend, test.payload), nil
			}
			store := newTestStore(t, files, fixedStateTime)
			_, err := store.ReadTransaction(t.Context(), TransactionBackend)
			if !errors.Is(err, test.want) {
				t.Fatalf("ReadTransaction() error = %v, want %v", err, test.want)
			}
			var readErr *ReadError
			if errors.As(err, &readErr) != test.wantReadErr {
				t.Fatalf(
					"ReadTransaction() ReadError = %t, want %t",
					errors.As(err, &readErr),
					test.wantReadErr,
				)
			}
			if test.wantReadErr && errors.Is(err, ErrNotFound) {
				t.Fatalf("ReadTransaction() error = %v, raw missing must not match ErrNotFound", err)
			}
		})
	}
}

func TestStore_ReadRejectsOversizedAndTrailingData(t *testing.T) {
	t.Parallel()

	t.Run("oversized", func(t *testing.T) {
		files := completeFakeStateFiles()
		files.read = func(
			context.Context,
			filesystem.StateFileKind,
			int64,
		) (stateFileSnapshot, error) {
			return nil, errors.Join(
				filesystem.ErrStateFileTooLarge,
				errors.New("bounded read rejected"),
			)
		}
		store := newTestStore(t, files, fixedStateTime)
		_, err := store.ReadTransaction(t.Context(), TransactionBackend)
		if !errors.Is(err, ErrCorrupt) ||
			!errors.Is(err, filesystem.ErrStateFileTooLarge) {
			t.Fatalf(
				"ReadTransaction() error = %v, want corrupt/size chain",
				err,
			)
		}
	})

	t.Run("second_value", func(t *testing.T) {
		files := completeFakeStateFiles()
		payload := append(
			mustStateJSON(t, validTransactionState(TransactionBackend)),
			[]byte("{}\n")...,
		)
		files.read = func(
			context.Context,
			filesystem.StateFileKind,
			int64,
		) (stateFileSnapshot, error) {
			return fakeSnapshot(filesystem.StateBackend, payload), nil
		}
		store := newTestStore(t, files, fixedStateTime)
		_, err := store.ReadTransaction(t.Context(), TransactionBackend)
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("ReadTransaction() error = %v, want ErrCorrupt", err)
		}
	})
}

func TestStore_ReadPreservesOrdinaryIOError(t *testing.T) {
	t.Parallel()

	cause := errors.New("sharing violation")
	files := completeFakeStateFiles()
	files.read = func(
		context.Context,
		filesystem.StateFileKind,
		int64,
	) (stateFileSnapshot, error) {
		return nil, cause
	}
	store := newTestStore(t, files, fixedStateTime)
	_, err := store.ReadEnvironment(t.Context())
	var readErr *ReadError
	if !errors.As(err, &readErr) || !errors.Is(err, cause) {
		t.Fatalf("ReadEnvironment() error = %v, want ReadError preserving cause", err)
	}
}

func TestStore_ReadChecksContextBeforeAndAfterIO(t *testing.T) {
	t.Parallel()

	t.Run("before", func(t *testing.T) {
		readCalls := 0
		files := completeFakeStateFiles()
		files.read = func(
			context.Context,
			filesystem.StateFileKind,
			int64,
		) (stateFileSnapshot, error) {
			readCalls++
			return nil, errFakeDependencyMissing
		}
		store := newTestStore(t, files, fixedStateTime)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := store.ReadEnvironment(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("ReadEnvironment() error = %v, want context.Canceled", err)
		}
		if readCalls != 0 {
			t.Fatalf("Read calls = %d, want 0", readCalls)
		}
	})

	t.Run("after", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		files := completeFakeStateFiles()
		files.read = func(
			context.Context,
			filesystem.StateFileKind,
			int64,
		) (stateFileSnapshot, error) {
			cancel()
			value := EnvironmentState{
				SchemaVersion:  SchemaVersion,
				Status:         protocol.StateReadyToStart,
				UpdatedAt:      fixedStateTime().UTC(),
				LastSuccessful: validLastSuccessful(),
				Broken:         nil,
			}
			return fakeSnapshot(
				filesystem.StateEnvironment,
				mustStateJSON(t, value),
			), nil
		}
		store := newTestStore(t, files, fixedStateTime)
		if _, err := store.ReadEnvironment(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("ReadEnvironment() error = %v, want context.Canceled", err)
		}
	})
}

func TestTransactionSnapshot_StateReturnsCopy(t *testing.T) {
	t.Parallel()

	want := validTransactionState(TransactionMutation)
	files := completeFakeStateFiles()
	files.read = func(
		context.Context,
		filesystem.StateFileKind,
		int64,
	) (stateFileSnapshot, error) {
		return fakeSnapshot(
			filesystem.StateMutation,
			mustStateJSON(t, want),
		), nil
	}
	store := newTestStore(t, files, fixedStateTime)
	snapshot, err := store.ReadTransaction(t.Context(), TransactionMutation)
	if err != nil {
		t.Fatalf("ReadTransaction() error = %v", err)
	}
	changed := snapshot.State()
	changed.OperationID = "01BX5ZZKBKACTAV9WEVGEMMVRZ"
	if got := snapshot.State(); got.OperationID != want.OperationID {
		t.Fatalf("snapshot State OperationID = %q, want %q", got.OperationID, want.OperationID)
	}
}
