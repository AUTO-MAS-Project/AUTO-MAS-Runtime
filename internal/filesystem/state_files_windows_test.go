package filesystem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestStateFiles_ReadMapsEveryKindThroughLayout(t *testing.T) {
	layout := newStateFilesTestLayout(t)
	if err := os.Mkdir(layout.StateDir(), 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	tests := []struct {
		name    string
		kind    StateFileKind
		path    string
		payload []byte
	}{
		{name: "backend", kind: StateBackend, path: layout.BackendStateFile(), payload: []byte("backend")},
		{name: "mutation", kind: StateMutation, path: layout.MutationStateFile(), payload: []byte("mutation")},
		{name: "update", kind: StateUpdate, path: layout.UpdateStateFile(), payload: []byte("update")},
		{name: "environment", kind: StateEnvironment, path: layout.EnvironmentStateFile(), payload: []byte("environment")},
	}
	for _, test := range tests {
		if err := os.WriteFile(test.path, test.payload, 0o600); err != nil {
			t.Fatalf("os.WriteFile(%q) error = %v", test.path, err)
		}
	}
	files, err := NewStateFiles(t.Context(), layout)
	if err != nil {
		t.Fatalf("NewStateFiles() error = %v", err)
	}
	t.Cleanup(func() { _ = files.Close() })
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot, err := files.Read(t.Context(), test.kind, MaxStateFileBytes)
			if err != nil {
				t.Fatalf("Read(%s) error = %v", test.kind, err)
			}
			if snapshot.Kind() != test.kind {
				t.Fatalf("Kind() = %s, want %s", snapshot.Kind(), test.kind)
			}
			if got := snapshot.Bytes(); !bytes.Equal(got, test.payload) {
				t.Fatalf("Bytes() = %q, want %q", got, test.payload)
			}
		})
	}
}

func TestStateFiles_ReadRejectsInvalidLimitBeforeIO(t *testing.T) {
	files, _ := newStateFilesFixture(t)
	var calls atomic.Int32
	api := files.api
	api.ntCreateRelative = func(windows.Handle, string, ntCreateSpec) (windows.Handle, error) {
		calls.Add(1)
		return windows.InvalidHandle, errors.New("unexpected gate I/O")
	}
	api.openRelative = func(windows.Handle, string, openSpec) (windows.Handle, error) {
		calls.Add(1)
		return windows.InvalidHandle, errors.New("unexpected leaf I/O")
	}
	api.identity = func(windows.Handle) (objectIdentity, error) {
		calls.Add(1)
		return objectIdentity{}, errors.New("unexpected identity I/O")
	}
	files.api = api

	tests := []struct {
		name     string
		kind     StateFileKind
		maxBytes int64
	}{
		{name: "zero", kind: StateBackend, maxBytes: 0},
		{name: "negative", kind: StateBackend, maxBytes: -1},
		{name: "over maximum", kind: StateBackend, maxBytes: MaxStateFileBytes + 1},
		{name: "invalid kind", kind: StateFileKind("foreign"), maxBytes: MaxStateFileBytes},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := files.Read(t.Context(), test.kind, test.maxBytes); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("Read() error = %v, want ErrInvalidArgument", err)
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("I/O calls = %d, want 0", got)
	}
}

func TestStateFiles_ReadRejectsOversizeBeforeAllocation(t *testing.T) {
	files, layout := newStateFilesFixture(t)
	payload := bytes.Repeat([]byte("x"), int(MaxStateFileBytes+1))
	if err := os.WriteFile(layout.BackendStateFile(), payload, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	var readCalls atomic.Int32
	readFile := files.api.readFile
	files.api.readFile = func(handle windows.Handle, buffer []byte) (int, error) {
		readCalls.Add(1)
		return readFile(handle, buffer)
	}
	if _, err := files.Read(t.Context(), StateBackend, MaxStateFileBytes); !errors.Is(err, ErrStateFileTooLarge) {
		t.Fatalf("Read() error = %v, want ErrStateFileTooLarge", err)
	}
	if got := readCalls.Load(); got != 0 {
		t.Fatalf("read calls = %d, want 0", got)
	}
}

func TestStateFiles_ReadDetectsGrowthPastLimit(t *testing.T) {
	files, layout := newStateFilesFixture(t)
	if err := os.WriteFile(layout.BackendStateFile(), []byte("small"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	readFile := files.api.readFile
	injected := false
	files.api.readFile = func(handle windows.Handle, buffer []byte) (int, error) {
		if !injected && len(buffer) == int(MaxStateFileBytes+1) {
			injected = true
			for i := range buffer {
				buffer[i] = 'g'
			}
			return len(buffer), nil
		}
		return readFile(handle, buffer)
	}
	if _, err := files.Read(t.Context(), StateBackend, MaxStateFileBytes); !errors.Is(err, ErrStateFileTooLarge) {
		t.Fatalf("Read() error = %v, want ErrStateFileTooLarge", err)
	}
	if !injected {
		t.Fatal("bounded maxBytes+1 read was not used")
	}
}

func TestStateFiles_ReadRejectsReparseAndHardLink(t *testing.T) {
	tests := []struct {
		name   string
		target stateTestLeaf
		attack string
	}{
		{name: "destination hardlink", target: stateTestDestination, attack: "hardlink"},
		{name: "guard hardlink", target: stateTestGuard, attack: "hardlink"},
		{name: "intent hardlink", target: stateTestIntent, attack: "hardlink"},
		{name: "backup hardlink", target: stateTestBackup, attack: "hardlink"},
		{name: "temp hardlink", target: stateTestTemp, attack: "hardlink"},
		{name: "destination reparse", target: stateTestDestination, attack: "reparse"},
		{name: "guard reparse", target: stateTestGuard, attack: "reparse"},
		{name: "intent reparse", target: stateTestIntent, attack: "reparse"},
		{name: "backup reparse", target: stateTestBackup, attack: "reparse"},
		{name: "temp reparse", target: stateTestTemp, attack: "reparse"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files, layout := newStateFilesFixture(t)
			fixture := newSealedStateFixture(t, files, layout, StateBackend, []byte("old"), []byte("new"))
			if test.target == stateTestBackup {
				fixture.installOldAtBackup(t)
			} else {
				fixture.installOldAtDestination(t)
			}
			fixture.installNewAtTemp(t)
			fixture.publishIntent(t)
			target := fixture.path(test.target)
			switch test.attack {
			case "hardlink":
				alias := target + ".alias"
				if err := os.Link(target, alias); err != nil {
					t.Skipf("hard-link fixture unavailable: %v", err)
				}
			case "reparse":
				if err := os.Remove(target); err != nil {
					t.Fatalf("os.Remove(%q) error = %v", target, err)
				}
				external := filepath.Join(t.TempDir(), "external")
				if err := os.WriteFile(external, []byte("external"), 0o600); err != nil {
					t.Fatalf("os.WriteFile() error = %v", err)
				}
				if err := os.Symlink(external, target); err != nil {
					t.Skipf("file symlink unavailable: %v", err)
				}
			default:
				t.Fatalf("unknown attack %q", test.attack)
			}
			if _, err := files.Read(t.Context(), StateBackend, MaxStateFileBytes); err == nil {
				t.Fatal("Read() accepted an unsafe protocol object")
			}
		})
	}
}

func TestStateFileSnapshot_BytesReturnsDefensiveCopy(t *testing.T) {
	files, layout := newStateFilesFixture(t)
	payload := []byte("immutable")
	if err := os.WriteFile(layout.BackendStateFile(), payload, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	snapshot, err := files.Read(t.Context(), StateBackend, MaxStateFileBytes)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	first := snapshot.Bytes()
	first[0] ^= 0xff
	second := snapshot.Bytes()
	if !bytes.Equal(second, payload) {
		t.Fatalf("second Bytes() = %q, want %q", second, payload)
	}
	if snapshot.owner != files.owner ||
		snapshot.volumeSerial == 0 ||
		snapshot.fileID == ([16]byte{}) ||
		snapshot.size != int64(len(payload)) ||
		snapshot.digest != sha256.Sum256(payload) {
		t.Fatalf("snapshot proof = %#v, want bound owner/kind/identity/size/digest", snapshot)
	}
}

func TestStateFiles_ReadIntentABADoesNotReturnNotFound(t *testing.T) {
	for _, outcome := range []string{"rollback", "commit"} {
		t.Run(outcome, func(t *testing.T) {
			layout := newStateFilesTestLayout(t)
			writer, err := NewStateFiles(t.Context(), layout)
			if err != nil {
				t.Fatalf("writer NewStateFiles() error = %v", err)
			}
			t.Cleanup(func() { _ = writer.Close() })
			waitEntered := make(chan struct{})
			releaseWait := make(chan struct{})
			readerAPI := newProductionPathAPI()
			reader, err := newStateFilesWithDependencies(
				t.Context(),
				layout,
				stateFileDependencies{
					api: readerAPI,
					waitGate: func(ctx context.Context, _ time.Duration) error {
						select {
						case <-waitEntered:
						default:
							close(waitEntered)
						}
						select {
						case <-releaseWait:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					},
				},
			)
			if err != nil {
				t.Fatalf("reader constructor error = %v", err)
			}
			t.Cleanup(func() { _ = reader.Close() })
			fixture := newSealedStateFixture(t, writer, layout, StateBackend, []byte("old"), []byte("new"))
			fixture.installOldAtDestination(t)
			fixture.installNewAtTemp(t)
			exclusive, err := writer.acquireStateGuard(t.Context(), StateBackend, stateGuardExclusive)
			if err != nil {
				t.Fatalf("exclusive guard error = %v", err)
			}
			fixture.publishIntent(t)
			if err := os.Remove(fixture.intentPath); err != nil {
				t.Fatalf("remove first intent error = %v", err)
			}
			fixture.publishIntent(t)
			if err := os.Rename(fixture.destinationPath, fixture.backupPath); err != nil {
				t.Fatalf("D -> B error = %v", err)
			}
			if _, err := os.Stat(fixture.destinationPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("raw D in gap error = %v, want os.ErrNotExist", err)
			}

			type readResult struct {
				snapshot StateFileSnapshot
				err      error
			}
			done := make(chan readResult, 1)
			go func() {
				snapshot, readErr := reader.Read(t.Context(), StateBackend, MaxStateFileBytes)
				done <- readResult{snapshot: snapshot, err: readErr}
			}()
			<-waitEntered
			select {
			case result := <-done:
				t.Fatalf("Read() returned in raw gap: %#v, %v", result.snapshot, result.err)
			default:
			}

			cancelCtx, cancel := context.WithCancel(t.Context())
			cancelDone := make(chan error, 1)
			go func() {
				_, readErr := reader.Read(cancelCtx, StateBackend, MaxStateFileBytes)
				cancelDone <- readErr
			}()
			cancel()
			if err := <-cancelDone; !errors.Is(err, context.Canceled) ||
				errors.Is(err, ErrStateFileNotFound) {
				t.Fatalf("canceled Read() error = %v, want context.Canceled only", err)
			}

			switch outcome {
			case "rollback":
				if err := os.Rename(fixture.backupPath, fixture.destinationPath); err != nil {
					t.Fatalf("B -> D error = %v", err)
				}
				if err := os.Remove(fixture.tempPath); err != nil {
					t.Fatalf("remove T error = %v", err)
				}
				if err := os.Remove(fixture.intentPath); err != nil {
					t.Fatalf("remove I error = %v", err)
				}
			case "commit":
				if err := os.Rename(fixture.tempPath, fixture.destinationPath); err != nil {
					t.Fatalf("T -> D error = %v", err)
				}
			}
			if err := writer.api.closeHandle(exclusive.handle); err != nil {
				t.Fatalf("close exclusive guard error = %v", err)
			}
			close(releaseWait)
			result := <-done
			if result.err != nil {
				t.Fatalf("Read() error = %v", result.err)
			}
			want := []byte("old")
			if outcome == "commit" {
				want = []byte("new")
			}
			if got := result.snapshot.Bytes(); !bytes.Equal(got, want) {
				t.Fatalf("Read() bytes = %q, want %q", got, want)
			}
		})
	}
}

func TestStateFiles_RecoveryMatrix(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(t *testing.T, fixture *sealedStateFixture)
		want        []byte
		wantMissing bool
		wantError   bool
		check       func(t *testing.T, fixture *sealedStateFixture)
	}{
		{
			name: "intent absent destination old",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOldAtDestination(t)
			},
			want: []byte("old"),
		},
		{
			name: "intent and destination absent",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOrphans(t)
			},
			wantMissing: true,
		},
		{
			name: "destination old backup absent temp new",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOldAtDestination(t)
				fixture.installNewAtTemp(t)
				fixture.publishIntent(t)
			},
			want: []byte("old"),
		},
		{
			name: "destination old backup and temp absent",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOldAtDestination(t)
				fixture.publishIntent(t)
			},
			want: []byte("old"),
		},
		{
			name: "destination absent backup old temp new",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOldAtBackup(t)
				fixture.installNewAtTemp(t)
				fixture.publishIntent(t)
			},
			want: []byte("old"),
		},
		{
			name: "foreign destination backup old temp new",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installForeignAtDestination(t)
				fixture.installOldAtBackup(t)
				fixture.installNewAtTemp(t)
				fixture.publishIntent(t)
				fixture.recordForeign(t)
			},
			want: []byte("old"),
			check: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.assertForeignUnchanged(t)
			},
		},
		{
			name: "opaque destination even with backup old",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOldAtBackup(t)
				fixture.installNewAtTemp(t)
				fixture.publishIntent(t)
				external := filepath.Join(t.TempDir(), "opaque")
				if err := os.WriteFile(external, []byte("opaque"), 0o600); err != nil {
					t.Fatalf("os.WriteFile() error = %v", err)
				}
				if err := os.Symlink(external, fixture.destinationPath); err != nil {
					t.Skipf("file symlink unavailable: %v", err)
				}
			},
			wantError: true,
		},
		{
			name: "destination new backup old temp absent",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installNewAtDestination(t)
				fixture.installOldAtBackup(t)
				fixture.publishIntent(t)
			},
			want: []byte("new"),
		},
		{
			name: "destination new backup and temp absent",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installNewAtDestination(t)
				fixture.publishIntent(t)
			},
			want: []byte("new"),
		},
		{
			name: "backup foreign",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOldAtDestination(t)
				if err := os.WriteFile(fixture.backupPath, []byte("foreign"), 0o600); err != nil {
					t.Fatalf("os.WriteFile() error = %v", err)
				}
				fixture.installNewAtTemp(t)
				fixture.publishIntent(t)
			},
			wantError: true,
		},
		{
			name: "fixed intent is not sealed",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOldAtDestination(t)
				if err := os.WriteFile(fixture.intentPath, []byte(`{"version":1}`), 0o600); err != nil {
					t.Fatalf("os.WriteFile() error = %v", err)
				}
			},
			wantError: true,
		},
		{
			name: "intent root mismatch",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOldAtDestination(t)
				fixture.installNewAtTemp(t)
				intent := fixture.intentValue(t)
				intent.Root.FileID[0] ^= 0xff
				fixture.publishIntentValue(t, intent)
			},
			wantError: true,
		},
		{
			name: "intent kind mismatch",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOldAtDestination(t)
				fixture.installNewAtTemp(t)
				intent := fixture.intentValue(t)
				intent.Kind = StateMutation
				fixture.publishIntentValue(t, intent)
			},
			wantError: true,
		},
		{
			name: "intent leaf mismatch",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOldAtDestination(t)
				fixture.installNewAtTemp(t)
				intent := fixture.intentValue(t)
				intent.IntentLeaf = stateIntentLeaf(StateMutation)
				fixture.publishIntentValue(t, intent)
			},
			wantError: true,
		},
		{
			name: "destination leaf mismatch",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOldAtDestination(t)
				fixture.installNewAtTemp(t)
				intent := fixture.intentValue(t)
				intent.DestinationLeaf = "mutation.json"
				fixture.publishIntentValue(t, intent)
			},
			wantError: true,
		},
		{
			name: "intent object ID mismatch",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOldAtDestination(t)
				fixture.installNewAtTemp(t)
				intent := fixture.intentValue(t)
				intent.IntentObject.FileID[0] ^= 0xff
				fixture.publishIntentValue(t, intent)
			},
			wantError: true,
		},
		{
			name: "old object size mismatch",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOldAtDestination(t)
				fixture.installNewAtTemp(t)
				intent := fixture.intentValue(t)
				intent.Old.Size++
				fixture.publishIntentValue(t, intent)
			},
			wantError: true,
		},
		{
			name: "old object digest mismatch",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOldAtDestination(t)
				fixture.installNewAtTemp(t)
				intent := fixture.intentValue(t)
				intent.Old.Digest[0] ^= 0xff
				fixture.publishIntentValue(t, intent)
			},
			wantError: true,
		},
		{
			name: "new object digest mismatch",
			setup: func(t *testing.T, fixture *sealedStateFixture) {
				fixture.installOldAtDestination(t)
				fixture.installNewAtTemp(t)
				intent := fixture.intentValue(t)
				intent.New.Digest[0] ^= 0xff
				fixture.publishIntentValue(t, intent)
			},
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files, layout := newStateFilesFixture(t)
			fixture := newSealedStateFixture(t, files, layout, StateBackend, []byte("old"), []byte("new"))
			test.setup(t, fixture)
			snapshot, err := files.Read(t.Context(), StateBackend, MaxStateFileBytes)
			switch {
			case test.wantMissing:
				if !errors.Is(err, ErrStateFileNotFound) ||
					errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("Read() error = %v, want dedicated ErrStateFileNotFound", err)
				}
			case test.wantError:
				if err == nil ||
					errors.Is(err, ErrStateFileNotFound) {
					t.Fatalf("Read() error = %v, want non-missing failure", err)
				}
			default:
				if err != nil {
					t.Fatalf("Read() error = %v", err)
				}
				if got := snapshot.Bytes(); !bytes.Equal(got, test.want) {
					t.Fatalf("Read() bytes = %q, want %q", got, test.want)
				}
			}
			if test.check != nil {
				test.check(t, fixture)
			}
		})
	}

	t.Run("raw not found never aliases stable missing", func(t *testing.T) {
		tests := []struct {
			name string
			err  error
		}{
			{name: "fs", err: fs.ErrNotExist},
			{name: "win32 name", err: windows.ERROR_FILE_NOT_FOUND},
			{name: "win32 path", err: windows.ERROR_PATH_NOT_FOUND},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				files, _ := newStateFilesFixture(t)
				files.api.ntCreateRelative = func(
					windows.Handle,
					string,
					ntCreateSpec,
				) (windows.Handle, error) {
					return windows.InvalidHandle, test.err
				}
				_, err := files.Read(t.Context(), StateBackend, MaxStateFileBytes)
				if !errors.Is(err, test.err) || errors.Is(err, ErrStateFileNotFound) {
					t.Fatalf("Read() error = %v, want raw error without stable sentinel", err)
				}
			})
		}
	})

	t.Run("stable missing close failure loses missing classification", func(t *testing.T) {
		files, _ := newStateFilesFixture(t)
		injected := errors.New("guard close failed")
		closeHandle := files.api.closeHandle
		files.api.closeHandle = func(handle windows.Handle) error {
			if path, err := files.api.finalPath(handle); err == nil &&
				filepath.Base(path) == stateGuardLeaf(StateBackend) {
				_ = closeHandle(handle)
				return injected
			}
			return closeHandle(handle)
		}
		_, err := files.Read(t.Context(), StateBackend, MaxStateFileBytes)
		if !errors.Is(err, injected) || errors.Is(err, ErrStateFileNotFound) {
			t.Fatalf("Read() error = %v, want close failure without missing sentinel", err)
		}
	})

	t.Run("unopenable destination is not inspectable foreign", func(t *testing.T) {
		files, layout := newStateFilesFixture(t)
		fixture := newSealedStateFixture(t, files, layout, StateBackend, []byte("old"), []byte("new"))
		fixture.installForeignAtDestination(t)
		fixture.installOldAtBackup(t)
		fixture.installNewAtTemp(t)
		fixture.publishIntent(t)
		openRelative := files.api.openRelative
		files.api.openRelative = func(
			parent windows.Handle,
			name string,
			spec openSpec,
		) (windows.Handle, error) {
			if name == filepath.Base(fixture.destinationPath) {
				return windows.InvalidHandle, windows.ERROR_ACCESS_DENIED
			}
			return openRelative(parent, name, spec)
		}
		if _, err := files.Read(
			t.Context(),
			StateBackend,
			MaxStateFileBytes,
		); err == nil || errors.Is(err, ErrStateFileNotFound) {
			t.Fatalf("Read() error = %v, want non-missing recovery failure", err)
		}
	})

	t.Run("unknown destination identity is not inspectable foreign", func(t *testing.T) {
		files, layout := newStateFilesFixture(t)
		fixture := newSealedStateFixture(t, files, layout, StateBackend, []byte("old"), []byte("new"))
		fixture.installForeignAtDestination(t)
		fixture.installOldAtBackup(t)
		fixture.installNewAtTemp(t)
		fixture.publishIntent(t)
		openRelative := files.api.openRelative
		identity := files.api.identity
		var destinationHandle windows.Handle
		files.api.openRelative = func(
			parent windows.Handle,
			name string,
			spec openSpec,
		) (windows.Handle, error) {
			handle, err := openRelative(parent, name, spec)
			if err == nil && name == filepath.Base(fixture.destinationPath) {
				destinationHandle = handle
			}
			return handle, err
		}
		files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
			if handle == destinationHandle && destinationHandle != windows.InvalidHandle {
				return objectIdentity{}, errors.New("injected destination identity failure")
			}
			return identity(handle)
		}
		if _, err := files.Read(
			t.Context(),
			StateBackend,
			MaxStateFileBytes,
		); err == nil || errors.Is(err, ErrStateFileNotFound) {
			t.Fatalf("Read() error = %v, want non-missing recovery failure", err)
		}
	})
}

func TestStateFiles_OrphanSidecarsAreIgnoredAndNotDeleted(t *testing.T) {
	tests := []struct {
		name        string
		destination bool
	}{
		{name: "destination exists", destination: true},
		{name: "destination absent"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files, layout := newStateFilesFixture(t)
			fixture := newSealedStateFixture(t, files, layout, StateBackend, []byte("old"), []byte("new"))
			if test.destination {
				fixture.installOldAtDestination(t)
			}
			fixture.installOrphans(t)
			beforeTemp, err := os.ReadFile(fixture.orphanTempPath)
			if err != nil {
				t.Fatalf("read orphan temp error = %v", err)
			}
			beforeIntent, err := os.ReadFile(fixture.orphanIntentPath)
			if err != nil {
				t.Fatalf("read orphan intent error = %v", err)
			}
			snapshot, readErr := files.Read(t.Context(), StateBackend, MaxStateFileBytes)
			if test.destination {
				if readErr != nil || !bytes.Equal(snapshot.Bytes(), []byte("old")) {
					t.Fatalf("Read() = %q, %v, want old/nil", snapshot.Bytes(), readErr)
				}
			} else if !errors.Is(readErr, ErrStateFileNotFound) {
				t.Fatalf("Read() error = %v, want ErrStateFileNotFound", readErr)
			}
			afterTemp, err := os.ReadFile(fixture.orphanTempPath)
			if err != nil || !bytes.Equal(afterTemp, beforeTemp) {
				t.Fatalf("orphan temp = %q, %v, want unchanged", afterTemp, err)
			}
			afterIntent, err := os.ReadFile(fixture.orphanIntentPath)
			if err != nil || !bytes.Equal(afterIntent, beforeIntent) {
				t.Fatalf("orphan intent = %q, %v, want unchanged", afterIntent, err)
			}
		})
	}
}

func TestStateFiles_GuardSerializesNamespaceDecisions(t *testing.T) {
	layout := newStateFilesTestLayout(t)
	first, err := NewStateFiles(t.Context(), layout)
	if err != nil {
		t.Fatalf("first NewStateFiles() error = %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := NewStateFiles(t.Context(), layout)
	if err != nil {
		t.Fatalf("second NewStateFiles() error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	specTests := []struct {
		name        string
		mode        stateGuardMode
		disposition uint32
		want        ntCreateSpec
	}{
		{
			name:        "shared create",
			mode:        stateGuardShared,
			disposition: ntFileCreate,
			want: ntCreateSpec{
				desiredAccess:     windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES,
				shareAccess:       windows.FILE_SHARE_READ,
				createDisposition: ntFileCreate,
				createOptions:     ntFileOpenReparsePoint | ntFileNonDirectoryFile,
			},
		},
		{
			name:        "shared open",
			mode:        stateGuardShared,
			disposition: ntFileOpen,
			want: ntCreateSpec{
				desiredAccess:     windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES,
				shareAccess:       windows.FILE_SHARE_READ,
				createDisposition: ntFileOpen,
				createOptions:     ntFileOpenReparsePoint | ntFileNonDirectoryFile,
			},
		},
		{
			name:        "exclusive create",
			mode:        stateGuardExclusive,
			disposition: ntFileCreate,
			want: ntCreateSpec{
				desiredAccess:     windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES,
				shareAccess:       0,
				createDisposition: ntFileCreate,
				createOptions:     ntFileOpenReparsePoint | ntFileNonDirectoryFile,
			},
		},
		{
			name:        "exclusive open",
			mode:        stateGuardExclusive,
			disposition: ntFileOpen,
			want: ntCreateSpec{
				desiredAccess:     windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES,
				shareAccess:       0,
				createDisposition: ntFileOpen,
				createOptions:     ntFileOpenReparsePoint | ntFileNonDirectoryFile,
			},
		},
	}
	for _, test := range specTests {
		t.Run(test.name, func(t *testing.T) {
			got, err := stateGuardNTSpec(test.mode, test.disposition)
			if err != nil {
				t.Fatalf("stateGuardNTSpec() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("stateGuardNTSpec() = %#v, want %#v", got, test.want)
			}
		})
	}

	valid, err := stateGuardNTSpec(stateGuardShared, ntFileOpen)
	if err != nil {
		t.Fatalf("stateGuardNTSpec() error = %v", err)
	}
	invalidSpecs := []struct {
		name string
		spec ntCreateSpec
		mode stateGuardMode
	}{
		{name: "attributes only", spec: withGuardAccess(valid, windows.FILE_READ_ATTRIBUTES), mode: stateGuardShared},
		{name: "zero access", spec: withGuardAccess(valid, 0), mode: stateGuardShared},
		{name: "synchronize", spec: withGuardAccess(valid, valid.desiredAccess|windows.SYNCHRONIZE), mode: stateGuardShared},
		{name: "write access", spec: withGuardAccess(valid, valid.desiredAccess|windows.FILE_WRITE_DATA), mode: stateGuardShared},
		{name: "delete access", spec: withGuardAccess(valid, valid.desiredAccess|windows.DELETE), mode: stateGuardShared},
		{name: "synchronous alert", spec: withGuardOptions(valid, valid.createOptions|ntFileSynchronousAlert), mode: stateGuardShared},
		{name: "synchronous nonalert", spec: withGuardOptions(valid, valid.createOptions|ntFileSynchronousNonalert), mode: stateGuardShared},
		{name: "missing reparse option", spec: withGuardOptions(valid, ntFileNonDirectoryFile), mode: stateGuardShared},
		{name: "missing nondirectory option", spec: withGuardOptions(valid, ntFileOpenReparsePoint), mode: stateGuardShared},
		{name: "open if", spec: withGuardDisposition(valid, ntFileOpenIf), mode: stateGuardShared},
		{name: "overwrite", spec: withGuardDisposition(valid, ntFileOverwrite), mode: stateGuardShared},
		{name: "overwrite if", spec: withGuardDisposition(valid, ntFileOverwriteIf), mode: stateGuardShared},
		{name: "supersede", spec: withGuardDisposition(valid, ntFileSupersede), mode: stateGuardShared},
		{name: "share write", spec: withGuardShare(valid, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE), mode: stateGuardShared},
		{name: "share delete", spec: withGuardShare(valid, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE), mode: stateGuardShared},
		{name: "reader share zero", spec: withGuardShare(valid, 0), mode: stateGuardShared},
		{name: "exclusive share read", spec: withGuardShare(valid, windows.FILE_SHARE_READ), mode: stateGuardExclusive},
	}
	for _, test := range invalidSpecs {
		t.Run("reject "+test.name, func(t *testing.T) {
			if err := validateStateGuardNTSpec(
				test.mode,
				ntFileOpen,
				test.spec,
			); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("validateStateGuardNTSpec() error = %v, want ErrInvalidArgument", err)
			}
		})
	}
	for _, test := range []struct {
		name        string
		expected    uint32
		disposition uint32
	}{
		{name: "create path uses open", expected: ntFileCreate, disposition: ntFileOpen},
		{name: "open path uses create", expected: ntFileOpen, disposition: ntFileCreate},
	} {
		t.Run("reject "+test.name, func(t *testing.T) {
			spec := valid
			spec.createDisposition = test.disposition
			if err := validateStateGuardNTSpec(
				stateGuardShared,
				test.expected,
				spec,
			); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("validateStateGuardNTSpec() error = %v, want ErrInvalidArgument", err)
			}
		})
	}

	var (
		specMu        sync.Mutex
		observedSpecs []ntCreateSpec
		createCalls   atomic.Int32
		createSuccess atomic.Int32
		openSuccess   atomic.Int32
	)
	createBarrier := make(chan struct{})
	wrapNTCreate := func(api *pathAPI) {
		original := api.ntCreateRelative
		api.ntCreateRelative = func(
			parent windows.Handle,
			name string,
			spec ntCreateSpec,
		) (windows.Handle, error) {
			specMu.Lock()
			observedSpecs = append(observedSpecs, spec)
			specMu.Unlock()
			if spec.createDisposition == ntFileCreate {
				if createCalls.Add(1) == 2 {
					close(createBarrier)
				}
				<-createBarrier
			}
			handle, createErr := original(parent, name, spec)
			if createErr == nil {
				switch spec.createDisposition {
				case ntFileCreate:
					createSuccess.Add(1)
				case ntFileOpen:
					openSuccess.Add(1)
				}
			}
			return handle, createErr
		}
	}
	wrapNTCreate(&first.api)
	wrapNTCreate(&second.api)
	type guardResult struct {
		files *StateFiles
		guard pinnedObject
		err   error
	}
	results := make(chan guardResult, 2)
	for _, files := range []*StateFiles{first, second} {
		go func(files *StateFiles) {
			guard, acquireErr := files.acquireStateGuard(
				t.Context(),
				StateBackend,
				stateGuardShared,
			)
			results <- guardResult{files: files, guard: guard, err: acquireErr}
		}(files)
	}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent shared acquire error = %v", result.err)
		}
		if err := result.files.api.closeHandle(result.guard.handle); err != nil {
			t.Fatalf("concurrent shared close error = %v", err)
		}
	}
	if got := createSuccess.Load(); got != 1 {
		t.Fatalf("successful guard creates = %d, want 1", got)
	}
	if got := openSuccess.Load(); got != 1 {
		t.Fatalf("successful guard opens = %d, want 1", got)
	}
	specMu.Lock()
	for _, got := range observedSpecs {
		mode := stateGuardExclusive
		if got.shareAccess == windows.FILE_SHARE_READ {
			mode = stateGuardShared
		}
		if err := validateStateGuardNTSpec(
			mode,
			got.createDisposition,
			got,
		); err != nil {
			specMu.Unlock()
			t.Fatalf("observed native guard spec = %#v: %v", got, err)
		}
	}
	specMu.Unlock()

	assertStateGuardIdentityFailures(t)
	assertGuardBlocksMode(t, first, second, stateGuardShared, stateGuardExclusive)
	assertGuardBlocksMode(t, first, second, stateGuardExclusive, stateGuardShared)
	assertGuardBlocksMode(t, first, second, stateGuardExclusive, stateGuardExclusive)

	guardPath := filepath.Join(layout.StateDir(), stateGuardLeaf(StateBackend))
	before, err := os.Stat(guardPath)
	if err != nil {
		t.Fatalf("os.Stat(G) error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	after, err := os.Stat(guardPath)
	if err != nil {
		t.Fatalf("os.Stat(G) after Close error = %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("persistent guard identity changed across instance Close")
	}
}

func assertStateGuardIdentityFailures(t *testing.T) {
	t.Helper()
	tests := []struct {
		name   string
		inject func(files *StateFiles)
	}{
		{
			name: "identity query",
			inject: func(files *StateFiles) {
				ntCreate := files.api.ntCreateRelative
				identity := files.api.identity
				var guardHandle windows.Handle
				files.api.ntCreateRelative = func(
					parent windows.Handle,
					name string,
					spec ntCreateSpec,
				) (windows.Handle, error) {
					handle, err := ntCreate(parent, name, spec)
					if err == nil {
						guardHandle = handle
					}
					return handle, err
				}
				files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
					if handle == guardHandle && guardHandle != windows.InvalidHandle {
						return objectIdentity{}, errors.New("injected guard identity failure")
					}
					return identity(handle)
				}
			},
		},
		{
			name: "volume mismatch",
			inject: func(files *StateFiles) {
				ntCreate := files.api.ntCreateRelative
				identity := files.api.identity
				var guardHandle windows.Handle
				files.api.ntCreateRelative = func(
					parent windows.Handle,
					name string,
					spec ntCreateSpec,
				) (windows.Handle, error) {
					handle, err := ntCreate(parent, name, spec)
					if err == nil {
						guardHandle = handle
					}
					return handle, err
				}
				files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
					got, err := identity(handle)
					if err == nil && handle == guardHandle && guardHandle != windows.InvalidHandle {
						got.volumeSerial++
					}
					return got, err
				}
			},
		},
		{
			name: "duplicate file ID mismatch",
			inject: func(files *StateFiles) {
				ntCreate := files.api.ntCreateRelative
				duplicateHandle := files.api.duplicateHandle
				identity := files.api.identity
				var guardHandle windows.Handle
				var duplicate windows.Handle
				files.api.ntCreateRelative = func(
					parent windows.Handle,
					name string,
					spec ntCreateSpec,
				) (windows.Handle, error) {
					handle, err := ntCreate(parent, name, spec)
					if err == nil {
						guardHandle = handle
					}
					return handle, err
				}
				files.api.duplicateHandle = func(handle windows.Handle) (windows.Handle, error) {
					got, err := duplicateHandle(handle)
					if err == nil && handle == guardHandle {
						duplicate = got
					}
					return got, err
				}
				files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
					got, err := identity(handle)
					if err == nil && handle == duplicate && duplicate != windows.InvalidHandle {
						got.fileID[0] ^= 0xff
					}
					return got, err
				}
			},
		},
		{
			name: "parent identity mismatch",
			inject: func(files *StateFiles) {
				openPath := files.api.openPath
				identity := files.api.identity
				var parentHandle windows.Handle
				files.api.openPath = func(path string, spec openSpec) (windows.Handle, error) {
					handle, err := openPath(path, spec)
					if err == nil && spec == parentIdentitySpec() {
						parentHandle = handle
					}
					return handle, err
				}
				files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
					got, err := identity(handle)
					if err == nil && handle == parentHandle && parentHandle != windows.InvalidHandle {
						got.fileID[0] ^= 0xff
					}
					return got, err
				}
			},
		},
	}
	for _, test := range tests {
		t.Run("guard rejects "+test.name, func(t *testing.T) {
			files, layout := newStateFilesFixture(t)
			test.inject(files)
			guard, err := files.acquireStateGuard(
				t.Context(),
				StateBackend,
				stateGuardShared,
			)
			if guard.handle != 0 && guard.handle != windows.InvalidHandle {
				_ = files.api.closeHandle(guard.handle)
			}
			if err == nil {
				t.Fatal("acquireStateGuard() accepted an unverifiable guard")
			}
			if _, statErr := os.Stat(
				filepath.Join(layout.StateDir(), stateGuardLeaf(StateBackend)),
			); statErr != nil {
				t.Fatalf("guard was removed after validation failure: %v", statErr)
			}
		})
	}
}

func TestStateFiles_CloseWaitsForReadAndRejectsNewRead(t *testing.T) {
	for _, point := range []string{"gate-wait", "open", "read", "leaf-close"} {
		t.Run(point, func(t *testing.T) {
			assertStateFilesCloseWaitsAt(t, point)
		})
	}
}

func TestNewStateFiles_CreatesAndPinsRuntimeStateDirectory(t *testing.T) {
	layout := newStateFilesTestLayout(t)
	if _, err := os.Stat(layout.StateDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("initial StateDir stat error = %v, want os.ErrNotExist", err)
	}
	files, err := NewStateFiles(t.Context(), layout)
	if err != nil {
		t.Fatalf("NewStateFiles() error = %v", err)
	}
	if information, err := os.Stat(layout.StateDir()); err != nil || !information.IsDir() {
		t.Fatalf("StateDir stat = %v, %v, want directory", information, err)
	}
	for _, path := range []string{layout.StateDir(), layout.AppRoot()} {
		renamed := path + "-renamed"
		if err := os.Rename(path, renamed); err == nil {
			_ = os.Rename(renamed, path)
			t.Fatalf("%q renamed while StateFiles pin was alive", path)
		}
	}
	if err := files.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	renamedState := layout.StateDir() + "-renamed"
	if err := os.Rename(layout.StateDir(), renamedState); err != nil {
		t.Fatalf("rename StateDir after Close error = %v", err)
	}
	if err := os.Rename(renamedState, layout.StateDir()); err != nil {
		t.Fatalf("restore StateDir error = %v", err)
	}
}

func TestNewStateFiles_VerificationFailureDoesNotRemoveCreatedDirectory(t *testing.T) {
	tests := []struct {
		name   string
		inject func(t *testing.T, layout *config.Layout, api *pathAPI)
	}{
		{
			name: "identity",
			inject: func(t *testing.T, layout *config.Layout, api *pathAPI) {
				t.Helper()
				openRelative := api.openRelative
				identity := api.identity
				var stateHandle windows.Handle
				api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
					handle, err := openRelative(parent, name, spec)
					if err == nil && spec.directory && name == filepath.Base(layout.StateDir()) {
						stateHandle = handle
					}
					return handle, err
				}
				api.identity = func(handle windows.Handle) (objectIdentity, error) {
					if handle == stateHandle && stateHandle != windows.InvalidHandle {
						return objectIdentity{}, errors.New("injected identity failure")
					}
					return identity(handle)
				}
			},
		},
		{
			name: "parent identity",
			inject: func(t *testing.T, layout *config.Layout, api *pathAPI) {
				t.Helper()
				openPath := api.openPath
				identity := api.identity
				var parentHandle windows.Handle
				api.openPath = func(path string, spec openSpec) (windows.Handle, error) {
					handle, err := openPath(path, spec)
					if err == nil && spec == parentIdentitySpec() &&
						filepath.Clean(path) == filepath.Clean(nativeWindowsPath(layout.AppRoot())) {
						parentHandle = handle
					}
					return handle, err
				}
				api.identity = func(handle windows.Handle) (objectIdentity, error) {
					got, err := identity(handle)
					if err == nil && handle == parentHandle && parentHandle != windows.InvalidHandle {
						got.fileID[0] ^= 0xff
					}
					return got, err
				}
			},
		},
		{
			name: "duplicate",
			inject: func(t *testing.T, _ *config.Layout, api *pathAPI) {
				t.Helper()
				api.duplicateHandle = func(windows.Handle) (windows.Handle, error) {
					return windows.InvalidHandle, errors.New("injected duplicate failure")
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout := newStateFilesTestLayout(t)
			api := newProductionPathAPI()
			test.inject(t, layout, &api)
			files, err := newStateFilesWithDependencies(
				t.Context(),
				layout,
				stateFileDependencies{api: api, waitGate: defaultStateGateWait},
			)
			if files != nil || err == nil {
				t.Fatalf("constructor = %#v, %v, want nil/error", files, err)
			}
			information, statErr := os.Stat(layout.StateDir())
			if statErr != nil || !information.IsDir() {
				t.Fatalf("StateDir after failure = %v, %v, want preserved directory", information, statErr)
			}
		})
	}
}

func TestStateFiles_CloseIsIdempotentAndRejectsNewRead(t *testing.T) {
	files, _ := newStateFilesFixture(t)
	injectedOne := errors.New("first pin close failed")
	injectedTwo := errors.New("second pin close failed")
	closeHandle := files.api.closeHandle
	var calls atomic.Int32
	files.api.closeHandle = func(handle windows.Handle) error {
		call := calls.Add(1)
		err := closeHandle(handle)
		switch call {
		case 1:
			return errors.Join(injectedOne, err)
		case 2:
			return errors.Join(injectedTwo, err)
		default:
			return err
		}
	}
	first := files.Close()
	if !errors.Is(first, injectedOne) || !errors.Is(first, injectedTwo) {
		t.Fatalf("first Close() error = %v, want both injected errors", first)
	}
	second := files.Close()
	if second != first {
		t.Fatalf("second Close() error = %v, want cached %v", second, first)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("close calls = %d, want 2", got)
	}
	if _, err := files.Read(t.Context(), StateBackend, MaxStateFileBytes); !errors.Is(err, ErrClosed) {
		t.Fatalf("Read() after Close error = %v, want ErrClosed", err)
	}
}

func TestStateFiles_ReadContextsRejectedBeforeIO(t *testing.T) {
	layout := newStateFilesTestLayout(t)
	api := newProductionPathAPI()
	var constructorCalls atomic.Int32
	attributes := api.attributes
	api.attributes = func(path string) (uint32, error) {
		constructorCalls.Add(1)
		return attributes(path)
	}
	if files, err := newStateFilesWithDependencies(
		nil,
		layout,
		stateFileDependencies{api: api, waitGate: defaultStateGateWait},
	); files != nil || !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil-context constructor = %#v, %v, want nil/ErrInvalidArgument", files, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if files, err := newStateFilesWithDependencies(
		ctx,
		layout,
		stateFileDependencies{api: api, waitGate: defaultStateGateWait},
	); files != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel constructor = %#v, %v, want nil/context.Canceled", files, err)
	}
	if got := constructorCalls.Load(); got != 0 {
		t.Fatalf("constructor I/O calls = %d, want 0", got)
	}

	files, _ := newStateFilesFixture(t)
	var readCalls atomic.Int32
	ntCreate := files.api.ntCreateRelative
	openRelative := files.api.openRelative
	readFile := files.api.readFile
	files.api.ntCreateRelative = func(parent windows.Handle, name string, spec ntCreateSpec) (windows.Handle, error) {
		readCalls.Add(1)
		return ntCreate(parent, name, spec)
	}
	files.api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
		readCalls.Add(1)
		return openRelative(parent, name, spec)
	}
	files.api.readFile = func(handle windows.Handle, buffer []byte) (int, error) {
		readCalls.Add(1)
		return readFile(handle, buffer)
	}
	if _, err := files.Read(nil, StateBackend, MaxStateFileBytes); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Read(nil) error = %v, want ErrInvalidArgument", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	if _, err := files.Read(ctx, StateBackend, MaxStateFileBytes); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read(pre-cancel) error = %v, want context.Canceled", err)
	}
	if got := readCalls.Load(); got != 0 {
		t.Fatalf("Read I/O calls = %d, want 0", got)
	}
}

type stateTestLeaf int

const (
	stateTestDestination stateTestLeaf = iota
	stateTestGuard
	stateTestIntent
	stateTestBackup
	stateTestTemp
)

type sealedStateFixture struct {
	files            *StateFiles
	kind             StateFileKind
	oldPayload       []byte
	newPayload       []byte
	foreignPayload   []byte
	destinationPath  string
	guardPath        string
	intentPath       string
	backupPath       string
	tempPath         string
	orphanTempPath   string
	orphanIntentPath string
	oldProof         stateObjectProof
	newProof         stateObjectProof
	foreignIdentity  objectIdentity
	foreignBytes     []byte
}

func newStateFilesTestLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	return layout
}

func newStateFilesFixture(t *testing.T) (*StateFiles, *config.Layout) {
	t.Helper()
	layout := newStateFilesTestLayout(t)
	files, err := NewStateFiles(t.Context(), layout)
	if err != nil {
		t.Fatalf("NewStateFiles() error = %v", err)
	}
	t.Cleanup(func() { _ = files.Close() })
	return files, layout
}

func newSealedStateFixture(
	t *testing.T,
	files *StateFiles,
	layout *config.Layout,
	kind StateFileKind,
	oldPayload []byte,
	newPayload []byte,
) *sealedStateFixture {
	t.Helper()
	return &sealedStateFixture{
		files:            files,
		kind:             kind,
		oldPayload:       append([]byte(nil), oldPayload...),
		newPayload:       append([]byte(nil), newPayload...),
		foreignPayload:   []byte("foreign"),
		destinationPath:  stateDestinationPath(layout, kind),
		guardPath:        filepath.Join(layout.StateDir(), stateGuardLeaf(kind)),
		intentPath:       filepath.Join(layout.StateDir(), stateIntentLeaf(kind)),
		backupPath:       filepath.Join(layout.StateDir(), fmt.Sprintf(".%s.backup-test", kind)),
		tempPath:         filepath.Join(layout.StateDir(), fmt.Sprintf(".%s.temp-test", kind)),
		orphanTempPath:   filepath.Join(layout.StateDir(), fmt.Sprintf(".%s.temp-orphan", kind)),
		orphanIntentPath: filepath.Join(layout.StateDir(), fmt.Sprintf(".%s.intent-orphan", kind)),
	}
}

func (f *sealedStateFixture) path(target stateTestLeaf) string {
	switch target {
	case stateTestDestination:
		return f.destinationPath
	case stateTestGuard:
		if _, err := f.files.Read(tContextWithoutFailure{}, f.kind, MaxStateFileBytes); err != nil &&
			!errors.Is(err, ErrStateFileNotFound) {
			panic(err)
		}
		return f.guardPath
	case stateTestIntent:
		return f.intentPath
	case stateTestBackup:
		return f.backupPath
	case stateTestTemp:
		return f.tempPath
	default:
		panic("unknown state test leaf")
	}
}

func (f *sealedStateFixture) installOldAtDestination(t *testing.T) {
	t.Helper()
	f.oldProof = writeStateProof(t, f.files, f.destinationPath, f.oldPayload)
}

func (f *sealedStateFixture) installOldAtBackup(t *testing.T) {
	t.Helper()
	f.oldProof = writeStateProof(t, f.files, f.backupPath, f.oldPayload)
}

func (f *sealedStateFixture) installNewAtTemp(t *testing.T) {
	t.Helper()
	f.newProof = writeStateProof(t, f.files, f.tempPath, f.newPayload)
}

func (f *sealedStateFixture) installNewAtDestination(t *testing.T) {
	t.Helper()
	f.newProof = writeStateProof(t, f.files, f.destinationPath, f.newPayload)
}

func (f *sealedStateFixture) installForeignAtDestination(t *testing.T) {
	t.Helper()
	_ = writeStateProof(t, f.files, f.destinationPath, f.foreignPayload)
}

func (f *sealedStateFixture) installOrphans(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(f.orphanTempPath, []byte("orphan-temp"), 0o600); err != nil {
		t.Fatalf("write orphan temp error = %v", err)
	}
	if err := os.WriteFile(f.orphanIntentPath, []byte("orphan-intent"), 0o600); err != nil {
		t.Fatalf("write orphan intent error = %v", err)
	}
}

func (f *sealedStateFixture) publishIntent(t *testing.T) {
	t.Helper()
	f.publishIntentValue(t, f.intentValue(t))
}

func (f *sealedStateFixture) intentValue(t *testing.T) stateIntent {
	t.Helper()
	if f.oldProof == (stateObjectProof{}) {
		f.oldProof = createAbsentStateProof(t, f.files, f.backupPath+".old-proof", f.oldPayload)
	}
	if f.newProof == (stateObjectProof{}) {
		f.newProof = createAbsentStateProof(t, f.files, f.tempPath+".new-proof", f.newPayload)
	}
	if err := os.WriteFile(f.intentPath, nil, 0o600); err != nil {
		t.Fatalf("create intent error = %v", err)
	}
	intentObject := identityForPath(t, f.files, f.intentPath)
	return stateIntent{
		Version:         stateIntentVersion,
		Kind:            f.kind,
		DestinationLeaf: filepath.Base(f.destinationPath),
		IntentLeaf:      filepath.Base(f.intentPath),
		TempLeaf:        filepath.Base(f.tempPath),
		BackupLeaf:      filepath.Base(f.backupPath),
		Nonce:           "test-nonce",
		Root:            proofIdentity(f.files.pins[1].identity),
		IntentObject:    proofIdentity(intentObject),
		Old:             f.oldProof,
		New:             f.newProof,
	}
}

func (f *sealedStateFixture) publishIntentValue(t *testing.T, intent stateIntent) {
	t.Helper()
	envelope, err := encodeStateIntent(intent)
	if err != nil {
		t.Fatalf("encodeStateIntent() error = %v", err)
	}
	if err := os.WriteFile(f.intentPath, envelope, 0o600); err != nil {
		t.Fatalf("write intent error = %v", err)
	}
}

func (f *sealedStateFixture) recordForeign(t *testing.T) {
	t.Helper()
	f.foreignIdentity = identityForPath(t, f.files, f.destinationPath)
	bytes, err := os.ReadFile(f.destinationPath)
	if err != nil {
		t.Fatalf("read foreign error = %v", err)
	}
	f.foreignBytes = bytes
}

func (f *sealedStateFixture) assertForeignUnchanged(t *testing.T) {
	t.Helper()
	gotIdentity := identityForPath(t, f.files, f.destinationPath)
	gotBytes, err := os.ReadFile(f.destinationPath)
	if err != nil {
		t.Fatalf("read foreign after Read error = %v", err)
	}
	if gotIdentity.fileID != f.foreignIdentity.fileID ||
		gotIdentity.volumeSerial != f.foreignIdentity.volumeSerial ||
		!bytes.Equal(gotBytes, f.foreignBytes) {
		t.Fatalf("foreign object changed: identity %#v -> %#v, bytes %q -> %q",
			f.foreignIdentity, gotIdentity, f.foreignBytes, gotBytes)
	}
}

func writeStateProof(
	t *testing.T,
	files *StateFiles,
	path string,
	payload []byte,
) stateObjectProof {
	t.Helper()
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
	identity := identityForPath(t, files, path)
	return stateObjectProof{
		stateIdentityProof: proofIdentity(identity),
		Size:               int64(len(payload)),
		Digest:             sha256.Sum256(payload),
	}
}

func createAbsentStateProof(
	t *testing.T,
	files *StateFiles,
	path string,
	payload []byte,
) stateObjectProof {
	t.Helper()
	proof := writeStateProof(t, files, path, payload)
	if err := os.Remove(path); err != nil {
		t.Fatalf("os.Remove(%q) error = %v", path, err)
	}
	return proof
}

func identityForPath(t *testing.T, files *StateFiles, path string) objectIdentity {
	t.Helper()
	handle, err := files.api.openPath(nativeWindowsPath(path), statePayloadReadSpec())
	if err != nil {
		t.Fatalf("openPath(%q) error = %v", path, err)
	}
	identity, identityErr := files.api.identity(handle)
	closeErr := files.api.closeHandle(handle)
	if identityErr != nil || closeErr != nil {
		t.Fatalf("identityForPath(%q) identity error = %v, close error = %v", path, identityErr, closeErr)
	}
	return identity
}

func proofIdentity(identity objectIdentity) stateIdentityProof {
	return stateIdentityProof{
		VolumeSerial: identity.volumeSerial,
		FileID:       identity.fileID,
	}
}

func stateDestinationPath(layout *config.Layout, kind StateFileKind) string {
	switch kind {
	case StateBackend:
		return layout.BackendStateFile()
	case StateMutation:
		return layout.MutationStateFile()
	case StateUpdate:
		return layout.UpdateStateFile()
	case StateEnvironment:
		return layout.EnvironmentStateFile()
	default:
		panic("invalid state kind")
	}
}

func withGuardAccess(spec ntCreateSpec, access uint32) ntCreateSpec {
	spec.desiredAccess = access
	return spec
}

func withGuardOptions(spec ntCreateSpec, options uint32) ntCreateSpec {
	spec.createOptions = options
	return spec
}

func withGuardDisposition(spec ntCreateSpec, disposition uint32) ntCreateSpec {
	spec.createDisposition = disposition
	return spec
}

func withGuardShare(spec ntCreateSpec, share uint32) ntCreateSpec {
	spec.shareAccess = share
	return spec
}

func assertGuardBlocksMode(
	t *testing.T,
	holder *StateFiles,
	waiter *StateFiles,
	holderMode stateGuardMode,
	waiterMode stateGuardMode,
) {
	t.Helper()
	held, err := holder.acquireStateGuard(t.Context(), StateBackend, holderMode)
	if err != nil {
		t.Fatalf("holder acquire error = %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	waiter.waitGate = func(ctx context.Context, _ time.Duration) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan error, 1)
	go func() {
		guard, acquireErr := waiter.acquireStateGuard(t.Context(), StateBackend, waiterMode)
		if acquireErr == nil {
			acquireErr = waiter.api.closeHandle(guard.handle)
		}
		done <- acquireErr
	}()
	<-entered
	select {
	case err := <-done:
		t.Fatalf("waiter acquired incompatible guard early: %v", err)
	default:
	}
	if err := holder.api.closeHandle(held.handle); err != nil {
		t.Fatalf("holder close error = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("waiter acquire after release error = %v", err)
	}
}

func assertStateFilesCloseWaitsAt(t *testing.T, point string) {
	t.Helper()
	layout := newStateFilesTestLayout(t)
	files, err := NewStateFiles(t.Context(), layout)
	if err != nil {
		t.Fatalf("NewStateFiles() error = %v", err)
	}
	if err := os.WriteFile(layout.BackendStateFile(), []byte("payload"), 0o600); err != nil {
		_ = files.Close()
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var blocker *StateFiles
	var held pinnedObject
	switch point {
	case "gate-wait":
		blocker, err = NewStateFiles(t.Context(), layout)
		if err != nil {
			_ = files.Close()
			t.Fatalf("blocker NewStateFiles() error = %v", err)
		}
		held, err = blocker.acquireStateGuard(t.Context(), StateBackend, stateGuardExclusive)
		if err != nil {
			_ = blocker.Close()
			_ = files.Close()
			t.Fatalf("exclusive guard error = %v", err)
		}
		files.waitGate = func(ctx context.Context, _ time.Duration) error {
			select {
			case <-entered:
			default:
				close(entered)
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	case "open":
		openRelative := files.api.openRelative
		files.api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
			if name == filepath.Base(layout.BackendStateFile()) {
				close(entered)
				<-release
			}
			return openRelative(parent, name, spec)
		}
	case "read":
		readFile := files.api.readFile
		var once sync.Once
		files.api.readFile = func(handle windows.Handle, buffer []byte) (int, error) {
			once.Do(func() {
				close(entered)
				<-release
			})
			return readFile(handle, buffer)
		}
	case "leaf-close":
		openRelative := files.api.openRelative
		closeHandle := files.api.closeHandle
		var payloadHandle windows.Handle
		files.api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
			handle, openErr := openRelative(parent, name, spec)
			if openErr == nil && name == filepath.Base(layout.BackendStateFile()) {
				payloadHandle = handle
			}
			return handle, openErr
		}
		files.api.closeHandle = func(handle windows.Handle) error {
			if handle == payloadHandle && payloadHandle != windows.InvalidHandle {
				close(entered)
				<-release
				payloadHandle = windows.InvalidHandle
			}
			return closeHandle(handle)
		}
	default:
		_ = files.Close()
		t.Fatalf("unknown point %q", point)
	}

	readDone := make(chan error, 1)
	go func() {
		_, readErr := files.Read(t.Context(), StateBackend, MaxStateFileBytes)
		readDone <- readErr
	}()
	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- files.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close() returned before %s completed: %v", point, err)
	default:
	}
	if point == "gate-wait" {
		if err := blocker.api.closeHandle(held.handle); err != nil {
			t.Fatalf("blocker guard close error = %v", err)
		}
	}
	close(release)
	if err := <-readDone; err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if blocker != nil {
		if err := blocker.Close(); err != nil {
			t.Fatalf("blocker Close() error = %v", err)
		}
	}
	if _, err := files.Read(t.Context(), StateBackend, MaxStateFileBytes); !errors.Is(err, ErrClosed) {
		t.Fatalf("Read() after Close error = %v, want ErrClosed", err)
	}
}

type tContextWithoutFailure struct{}

func (tContextWithoutFailure) Deadline() (time.Time, bool) { return time.Time{}, false }
func (tContextWithoutFailure) Done() <-chan struct{}       { return nil }
func (tContextWithoutFailure) Err() error                  { return nil }
func (tContextWithoutFailure) Value(any) any               { return nil }

var _ context.Context = tContextWithoutFailure{}
var _ = sync.Mutex{}
