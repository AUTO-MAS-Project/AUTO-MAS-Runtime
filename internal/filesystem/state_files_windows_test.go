package filesystem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestStateFiles_ReadFailsClosedOnInjectedUnsafeCandidates(t *testing.T) {
	tests := []struct {
		name   string
		inject func(t *testing.T, files *StateFiles, fixture *sealedStateFixture)
	}{
		{
			name: "reparse",
			inject: func(t *testing.T, files *StateFiles, fixture *sealedStateFixture) {
				t.Helper()
				openRelative := files.api.openRelative
				identity := files.api.identity
				var destination windows.Handle
				files.api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
					handle, err := openRelative(parent, name, spec)
					if err == nil && name == filepath.Base(fixture.destinationPath) {
						destination = handle
					}
					return handle, err
				}
				files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
					got, err := identity(handle)
					if err == nil && handle == destination && destination != windows.InvalidHandle {
						got.attributes |= windows.FILE_ATTRIBUTE_REPARSE_POINT
					}
					return got, err
				}
			},
		},
		{
			name: "opaque",
			inject: func(t *testing.T, files *StateFiles, fixture *sealedStateFixture) {
				t.Helper()
				openRelative := files.api.openRelative
				files.api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
					if name == filepath.Base(fixture.destinationPath) {
						return windows.InvalidHandle, windows.ERROR_ACCESS_DENIED
					}
					return openRelative(parent, name, spec)
				}
			},
		},
		{
			name: "digest unknown",
			inject: func(t *testing.T, files *StateFiles, fixture *sealedStateFixture) {
				t.Helper()
				openRelative := files.api.openRelative
				readFile := files.api.readFile
				var destination windows.Handle
				files.api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
					handle, err := openRelative(parent, name, spec)
					if err == nil && name == filepath.Base(fixture.destinationPath) {
						destination = handle
					}
					return handle, err
				}
				files.api.readFile = func(handle windows.Handle, buffer []byte) (int, error) {
					if handle == destination && destination != windows.InvalidHandle {
						return 0, errors.New("injected digest failure")
					}
					return readFile(handle, buffer)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files, layout := newStateFilesFixture(t)
			fixture := newSealedStateFixture(t, files, layout, StateBackend, []byte("old"), []byte("new"))
			fixture.installForeignAtDestination(t)
			fixture.installOldAtBackup(t)
			fixture.installNewAtTemp(t)
			fixture.publishIntent(t)
			test.inject(t, files, fixture)
			if _, err := files.Read(t.Context(), StateBackend, MaxStateFileBytes); err == nil ||
				errors.Is(err, ErrStateFileNotFound) {
				t.Fatalf("Read() error = %v, want closed non-missing failure", err)
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
					fillNonce: fillCryptoNonce,
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
			waitStateTestSignal(t, waitEntered, "reader guard wait")
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
			if err := waitStateTestResult(t, cancelDone, "canceled reader completion"); !errors.Is(err, context.Canceled) ||
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
			result := waitStateTestResult(t, done, "reader completion")
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
				fixture.publishUncheckedIntentValue(t, intent)
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
				fixture.publishUncheckedIntentValue(t, intent)
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
				fixture.publishUncheckedIntentValue(t, intent)
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

	t.Run("post-open not found never aliases stable missing", func(t *testing.T) {
		tests := []struct {
			name   string
			inject func(files *StateFiles)
		}{
			{
				name: "identity",
				inject: func(files *StateFiles) {
					identity := files.api.identity
					var destination windows.Handle
					openRelative := files.api.openRelative
					files.api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
						handle, err := openRelative(parent, name, spec)
						if err == nil && name == "backend.json" {
							destination = handle
						}
						return handle, err
					}
					files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
						if handle == destination && destination != windows.InvalidHandle {
							return objectIdentity{}, windows.ERROR_FILE_NOT_FOUND
						}
						return identity(handle)
					}
				},
			},
			{
				name: "final path",
				inject: func(files *StateFiles) {
					finalPath := files.api.finalPath
					var destination windows.Handle
					openRelative := files.api.openRelative
					files.api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
						handle, err := openRelative(parent, name, spec)
						if err == nil && name == "backend.json" {
							destination = handle
						}
						return handle, err
					}
					files.api.finalPath = func(handle windows.Handle) (string, error) {
						if handle == destination && destination != windows.InvalidHandle {
							return "", windows.ERROR_FILE_NOT_FOUND
						}
						return finalPath(handle)
					}
				},
			},
			{
				name: "canonicalize",
				inject: func(files *StateFiles) {
					finalPath := files.api.finalPath
					var destination windows.Handle
					openRelative := files.api.openRelative
					files.api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
						handle, err := openRelative(parent, name, spec)
						if err == nil && name == "backend.json" {
							destination = handle
						}
						return handle, err
					}
					files.api.finalPath = func(handle windows.Handle) (string, error) {
						if handle == destination && destination != windows.InvalidHandle {
							return `Z:\missing\backend.json`, nil
						}
						return finalPath(handle)
					}
				},
			},
			{
				name: "parent",
				inject: func(files *StateFiles) {
					openPath := files.api.openPath
					files.api.openPath = func(path string, spec openSpec) (windows.Handle, error) {
						if spec == parentIdentitySpec() {
							return windows.InvalidHandle, windows.ERROR_FILE_NOT_FOUND
						}
						return openPath(path, spec)
					}
				},
			},
			{
				name: "cleanup",
				inject: func(files *StateFiles) {
					identity := files.api.identity
					closeHandle := files.api.closeHandle
					var destination windows.Handle
					openRelative := files.api.openRelative
					files.api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
						handle, err := openRelative(parent, name, spec)
						if err == nil && name == "backend.json" {
							destination = handle
						}
						return handle, err
					}
					files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
						if handle == destination && destination != windows.InvalidHandle {
							return objectIdentity{}, errors.New("force cleanup")
						}
						return identity(handle)
					}
					files.api.closeHandle = func(handle windows.Handle) error {
						if handle == destination && destination != windows.InvalidHandle {
							_ = closeHandle(handle)
							return windows.ERROR_FILE_NOT_FOUND
						}
						return closeHandle(handle)
					}
				},
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				files, layout := newStateFilesFixture(t)
				if err := os.WriteFile(layout.BackendStateFile(), []byte("payload"), 0o600); err != nil {
					t.Fatalf("os.WriteFile() error = %v", err)
				}
				test.inject(files)
				_, err := files.Read(t.Context(), StateBackend, MaxStateFileBytes)
				if (!errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND)) ||
					errors.Is(err, ErrStateFileNotFound) {
					t.Fatalf("Read() error = %v, want raw post-open not-found", err)
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

	t.Run("WriteAtomic recovers pre-commit gap before next write", func(t *testing.T) {
		files, layout := newStateFilesFixture(t)
		fixture := newSealedStateFixture(t, files, layout, StateBackend, []byte("old"), []byte("new"))
		fixture.installOldAtBackup(t)
		fixture.installNewAtTemp(t)
		fixture.publishIntent(t)

		result, err := files.WriteAtomic(t.Context(), StateBackend, []byte("next"))
		if err != nil {
			t.Fatalf("WriteAtomic() error = %v", err)
		}
		if result != (WriteAtomicResult{MutationApplied: true}) {
			t.Fatalf("WriteAtomic() result = %#v, want applied without recovery", result)
		}
		snapshot, err := files.Read(t.Context(), StateBackend, MaxStateFileBytes)
		if err != nil {
			t.Fatalf("Read() after recovery error = %v", err)
		}
		if got := snapshot.Bytes(); !bytes.Equal(got, []byte("next")) {
			t.Fatalf("Read() after recovery = %q, want next", got)
		}
	})
}

func TestStateFiles_GuardWaitCancellationPreservesFailureContext(t *testing.T) {
	files, _ := newStateFilesFixture(t)
	injected := windows.ERROR_SHARING_VIOLATION
	files.api.ntCreateRelative = func(windows.Handle, string, ntCreateSpec) (windows.Handle, error) {
		return windows.InvalidHandle, injected
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	files.waitGate = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	_, err := files.acquireStateGuard(ctx, StateBackend, stateGuardShared)
	var fileErr *FileError
	if !errors.As(err, &fileErr) || !errors.Is(err, injected) || !errors.Is(err, context.Canceled) {
		t.Fatalf("acquireStateGuard() error = %v, want FileError + sharing violation + context.Canceled", err)
	}
}

func TestValidateStateIntentValue_RejectsUnboundTransactionLeaves(t *testing.T) {
	valid := stateIntent{
		Version:         stateIntentVersion,
		Kind:            StateBackend,
		DestinationLeaf: "backend.json",
		IntentLeaf:      stateIntentLeaf(StateBackend),
		TempLeaf:        ".backend.temp.0123456789abcdef0123456789abcdef",
		BackupLeaf:      ".backend.backup.0123456789abcdef0123456789abcdef",
		Nonce:           "0123456789abcdef0123456789abcdef",
		Root:            stateIdentityProof{VolumeSerial: 1, FileID: [16]byte{1}},
		IntentObject:    stateIdentityProof{VolumeSerial: 1, FileID: [16]byte{2}},
		Old: stateObjectProof{
			stateIdentityProof: stateIdentityProof{VolumeSerial: 1, FileID: [16]byte{3}},
			Digest:             [32]byte{1},
		},
		New: stateObjectProof{
			stateIdentityProof: stateIdentityProof{VolumeSerial: 1, FileID: [16]byte{4}},
			Digest:             [32]byte{2},
		},
	}
	tests := []struct {
		name   string
		mutate func(*stateIntent)
	}{
		{name: "guard", mutate: func(intent *stateIntent) { intent.TempLeaf = stateGuardLeaf(StateBackend) }},
		{name: "fixed destination", mutate: func(intent *stateIntent) { intent.TempLeaf = intent.DestinationLeaf }},
		{name: "fixed intent", mutate: func(intent *stateIntent) { intent.BackupLeaf = intent.IntentLeaf }},
		{name: "cross kind", mutate: func(intent *stateIntent) { intent.TempLeaf = ".mutation.temp.0123456789abcdef0123456789abcdef" }},
		{name: "swapped role", mutate: func(intent *stateIntent) { intent.TempLeaf = ".backend.backup.0123456789abcdef0123456789abcdef" }},
		{name: "wrong nonce", mutate: func(intent *stateIntent) { intent.BackupLeaf = ".backend.backup.fedcba9876543210fedcba9876543210" }},
		{name: "short nonce", mutate: func(intent *stateIntent) { intent.Nonce = "0123456789abcdef" }},
		{name: "non-hex nonce", mutate: func(intent *stateIntent) { intent.Nonce = "0123456789abcdef0123456789abcdeg" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			intent := valid
			test.mutate(&intent)
			if err := validateStateIntentValue(intent); err == nil {
				t.Fatal("validateStateIntentValue() accepted an unbound transaction leaf")
			}
		})
	}
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
				select {
				case <-createBarrier:
				case <-t.Context().Done():
					return windows.InvalidHandle, t.Context().Err()
				}
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
	sharedGuards := make([]guardResult, 0, 2)
	for range 2 {
		result := waitStateTestResult(t, results, "shared guard acquire")
		if result.err != nil {
			t.Fatalf("concurrent shared acquire error = %v", result.err)
		}
		sharedGuards = append(sharedGuards, result)
	}
	for _, result := range sharedGuards {
		if _, err := result.files.api.identity(result.guard.handle); err != nil {
			t.Fatalf("shared guard handle did not remain live: %v", err)
		}
	}
	for _, result := range sharedGuards {
		if err := result.files.api.closeHandle(result.guard.handle); err != nil {
			t.Fatalf("concurrent shared close error = %v", err)
		}
	}
	exclusive, err := first.acquireStateGuard(t.Context(), StateMutation, stateGuardExclusive)
	if err != nil {
		t.Fatalf("exclusive create acquire error = %v", err)
	}
	if err := first.api.closeHandle(exclusive.handle); err != nil {
		t.Fatalf("exclusive create close error = %v", err)
	}
	exclusive, err = second.acquireStateGuard(t.Context(), StateMutation, stateGuardExclusive)
	if err != nil {
		t.Fatalf("exclusive open acquire error = %v", err)
	}
	if err := second.api.closeHandle(exclusive.handle); err != nil {
		t.Fatalf("exclusive open close error = %v", err)
	}
	if got := createSuccess.Load(); got != 2 {
		t.Fatalf("successful guard creates = %d, want 2", got)
	}
	if got := openSuccess.Load(); got != 2 {
		t.Fatalf("successful guard opens = %d, want 2", got)
	}
	specMu.Lock()
	observedCounts := make(map[string]int, 4)
	for _, got := range observedSpecs {
		mode := stateGuardExclusive
		if got.shareAccess == windows.FILE_SHARE_READ {
			mode = stateGuardShared
		}
		observedCounts[fmt.Sprintf("%d/%d", mode, got.createDisposition)]++
		if err := validateStateGuardNTSpec(
			mode,
			got.createDisposition,
			got,
		); err != nil {
			specMu.Unlock()
			t.Fatalf("observed native guard spec = %#v: %v", got, err)
		}
	}
	for _, test := range specTests {
		key := fmt.Sprintf("%d/%d", test.mode, test.disposition)
		if observedCounts[key] == 0 {
			specMu.Unlock()
			t.Fatalf("production guard did not use native parameters for %s", test.name)
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
				stateFileDependencies{api: api, waitGate: defaultStateGateWait, fillNonce: fillCryptoNonce},
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
		stateFileDependencies{api: api, waitGate: defaultStateGateWait, fillNonce: fillCryptoNonce},
	); files != nil || !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil-context constructor = %#v, %v, want nil/ErrInvalidArgument", files, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if files, err := newStateFilesWithDependencies(
		ctx,
		layout,
		stateFileDependencies{api: api, waitGate: defaultStateGateWait, fillNonce: fillCryptoNonce},
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

func TestStateFiles_WriteAtomicRejectsEmptyAndOversizeBeforeIO(t *testing.T) {
	files, _ := newStateFilesFixture(t)
	var calls atomic.Int32
	files.api.ntCreateRelative = func(windows.Handle, string, ntCreateSpec) (windows.Handle, error) {
		calls.Add(1)
		return windows.InvalidHandle, errors.New("unexpected gate I/O")
	}
	files.api.openRelative = func(windows.Handle, string, openSpec) (windows.Handle, error) {
		calls.Add(1)
		return windows.InvalidHandle, errors.New("unexpected leaf I/O")
	}
	files.api.writeFile = func(windows.Handle, []byte) (int, error) {
		calls.Add(1)
		return 0, errors.New("unexpected write I/O")
	}
	files.api.setStateDisposition = func(windows.Handle, stateDispositionSpec) error {
		calls.Add(1)
		return errors.New("unexpected disposition I/O")
	}
	files.api.renameState = func(windows.Handle, windows.Handle, string, uint32) error {
		calls.Add(1)
		return errors.New("unexpected rename I/O")
	}

	tests := []struct {
		name    string
		ctx     context.Context
		kind    StateFileKind
		payload []byte
		want    error
	}{
		{name: "nil context", kind: StateBackend, payload: []byte("value"), want: ErrInvalidArgument},
		{name: "nil payload", ctx: t.Context(), kind: StateBackend, want: ErrInvalidArgument},
		{name: "empty payload", ctx: t.Context(), kind: StateBackend, payload: []byte{}, want: ErrInvalidArgument},
		{name: "oversize", ctx: t.Context(), kind: StateBackend, payload: make([]byte, MaxStateFileBytes+1), want: ErrInvalidArgument},
		{name: "invalid kind", ctx: t.Context(), kind: StateFileKind("foreign"), payload: []byte("value"), want: ErrInvalidArgument},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := files.WriteAtomic(test.ctx, test.kind, test.payload)
			if result != (WriteAtomicResult{}) || !errors.Is(err, test.want) {
				t.Fatalf("WriteAtomic() = %#v, %v, want zero/%v", result, err, test.want)
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("I/O calls = %d, want 0", got)
	}
}

func TestStateFiles_WriteAtomicCopiesCallerPayload(t *testing.T) {
	files, _ := newStateFilesFixture(t)
	payload := []byte("caller-owned")
	result, err := files.WriteAtomic(t.Context(), StateBackend, payload)
	if err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
	if result != (WriteAtomicResult{MutationApplied: true}) {
		t.Fatalf("WriteAtomic() result = %#v, want applied without recovery", result)
	}
	payload[0] ^= 0xff
	snapshot, err := files.Read(t.Context(), StateBackend, MaxStateFileBytes)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got := snapshot.Bytes(); !bytes.Equal(got, []byte("caller-owned")) {
		t.Fatalf("Read() = %q, want caller-owned", got)
	}
}

func TestStateFiles_WriteAtomicReturnsTypedStateWriteErrors(t *testing.T) {
	tests := []struct {
		name  string
		phase StateWritePhase
		setup func(t *testing.T, files *StateFiles, layout *config.Layout)
	}{
		{
			name:  "recover",
			phase: StateWritePhaseRecover,
			setup: func(t *testing.T, files *StateFiles, _ *config.Layout) {
				t.Helper()
				files.api.ntCreateRelative = func(windows.Handle, string, ntCreateSpec) (windows.Handle, error) {
					return windows.InvalidHandle, errors.New("injected recover failure")
				}
			},
		},
		{
			name:  "create",
			phase: StateWritePhaseCreate,
			setup: func(t *testing.T, files *StateFiles, _ *config.Layout) {
				t.Helper()
				if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("prime")); err != nil {
					t.Fatalf("prime WriteAtomic() error = %v", err)
				}
				openRelative := files.api.openRelative
				files.api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
					if spec.creation == windows.CREATE_NEW {
						return windows.InvalidHandle, errors.New("injected create failure")
					}
					return openRelative(parent, name, spec)
				}
			},
		},
		{
			name:  "write",
			phase: StateWritePhaseWrite,
			setup: func(t *testing.T, files *StateFiles, _ *config.Layout) {
				t.Helper()
				if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("prime")); err != nil {
					t.Fatalf("prime WriteAtomic() error = %v", err)
				}
				files.api.writeFile = func(windows.Handle, []byte) (int, error) {
					return 0, errors.New("injected write failure")
				}
			},
		},
		{
			name:  "sync",
			phase: StateWritePhaseSync,
			setup: func(t *testing.T, files *StateFiles, _ *config.Layout) {
				t.Helper()
				if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("prime")); err != nil {
					t.Fatalf("prime WriteAtomic() error = %v", err)
				}
				files.api.flushFile = func(windows.Handle) error {
					return errors.New("injected sync failure")
				}
			},
		},
		{
			name:  "rename",
			phase: StateWritePhaseRename,
			setup: func(t *testing.T, files *StateFiles, _ *config.Layout) {
				t.Helper()
				if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("prime")); err != nil {
					t.Fatalf("prime WriteAtomic() error = %v", err)
				}
				files.api.renameState = func(windows.Handle, windows.Handle, string, uint32) error {
					return errors.New("injected rename failure")
				}
			},
		},
		{
			name:  "finalize",
			phase: StateWritePhaseFinalize,
			setup: func(t *testing.T, files *StateFiles, _ *config.Layout) {
				t.Helper()
				if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("prime")); err != nil {
					t.Fatalf("prime WriteAtomic() error = %v", err)
				}
				files.api.setStateDisposition = func(windows.Handle, stateDispositionSpec) error {
					return errors.New("injected finalize failure")
				}
			},
		},
		{
			name:  "close",
			phase: StateWritePhaseClose,
			setup: func(t *testing.T, files *StateFiles, _ *config.Layout) {
				t.Helper()
				ntCreateRelative := files.api.ntCreateRelative
				var guardHandle windows.Handle
				files.api.ntCreateRelative = func(
					parent windows.Handle,
					name string,
					spec ntCreateSpec,
				) (windows.Handle, error) {
					handle, err := ntCreateRelative(parent, name, spec)
					if err == nil && name == stateGuardLeaf(StateBackend) {
						guardHandle = handle
					}
					return handle, err
				}
				closeHandle := files.api.closeHandle
				var injected atomic.Bool
				files.api.closeHandle = func(handle windows.Handle) error {
					err := closeHandle(handle)
					if handle == guardHandle &&
						injected.CompareAndSwap(false, true) {
						return errors.Join(errors.New("injected close failure"), err)
					}
					return err
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files, layout := newStateFilesFixture(t)
			test.setup(t, files, layout)
			result, err := files.WriteAtomic(t.Context(), StateBackend, []byte("next"))
			var writeErr *StateWriteError
			if !errors.As(err, &writeErr) {
				t.Fatalf("WriteAtomic() error = %v, want *StateWriteError", err)
			}
			if writeErr.Phase != test.phase {
				t.Fatalf(
					"StateWriteError.Phase = %s, want %s; error = %v",
					writeErr.Phase,
					test.phase,
					err,
				)
			}
			if result.MutationApplied != writeErr.MutationApplied ||
				result.RecoveryRequired != writeErr.RecoveryRequired {
				t.Fatalf("result/error facts differ: %#v / %#v", result, writeErr)
			}
			if writeErr.Cause == nil && writeErr.CleanupError == nil {
				t.Fatal("StateWriteError lost both error chains")
			}
		})
	}
}

func TestStateFiles_IntentPublicationIsSealedAndNoReplace(t *testing.T) {
	t.Run("published intent is canonical and rename flags are zero", func(t *testing.T) {
		files, layout := newStateFilesFixture(t)
		if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("old")); err != nil {
			t.Fatalf("initial WriteAtomic() error = %v", err)
		}
		renameState := files.api.renameState
		var sawIntent atomic.Bool
		files.api.renameState = func(source windows.Handle, parent windows.Handle, name string, flags uint32) error {
			if flags != 0 {
				t.Fatalf("rename flags = %#x, want 0", flags)
			}
			if name == stateIntentLeaf(StateBackend) {
				if _, err := windows.SetFilePointer(source, 0, nil, windows.FILE_BEGIN); err != nil {
					t.Fatalf("rewind sealed staging intent error = %v", err)
				}
				payload := make([]byte, maxStateIntentBytes+1)
				total := 0
				for total < len(payload) {
					n, err := files.api.readFile(source, payload[total:])
					if err != nil {
						t.Fatalf("read sealed staging intent error = %v", err)
					}
					total += n
					if n == 0 {
						break
					}
				}
				if _, err := decodeStateIntent(payload[:total]); err != nil {
					t.Fatalf("decode sealed staging intent error = %v", err)
				}
				if _, err := os.Stat(filepath.Join(layout.StateDir(), name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("fixed intent before publication error = %v, want not-exist", err)
				}
				sawIntent.Store(true)
			}
			return renameState(source, parent, name, flags)
		}
		if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("new")); err != nil {
			t.Fatalf("replacement WriteAtomic() error = %v", err)
		}
		if !sawIntent.Load() {
			t.Fatal("sealed intent publication was not observed")
		}
	})

	t.Run("existing fixed intent is never replaced", func(t *testing.T) {
		files, layout := newStateFilesFixture(t)
		if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("old")); err != nil {
			t.Fatalf("initial WriteAtomic() error = %v", err)
		}
		intentPath := filepath.Join(layout.StateDir(), stateIntentLeaf(StateBackend))
		competitor := []byte("competitor")
		if err := os.WriteFile(intentPath, competitor, 0o600); err != nil {
			t.Fatalf("write competitor intent error = %v", err)
		}
		before := identityForPath(t, files, intentPath)
		result, err := files.WriteAtomic(t.Context(), StateBackend, []byte("new"))
		if err == nil || result.MutationApplied {
			t.Fatalf("WriteAtomic() = %#v, %v, want pre-commit failure", result, err)
		}
		after := identityForPath(t, files, intentPath)
		got, readErr := os.ReadFile(intentPath)
		if readErr != nil || !bytes.Equal(got, competitor) || before.fileID != after.fileID {
			t.Fatalf("competitor intent changed: bytes %q/%v, identity %#v -> %#v", got, readErr, before, after)
		}
	})
}

func TestStateFiles_RecoveryNeverOverwritesForeignObjects(t *testing.T) {
	files, layout := newStateFilesFixture(t)
	if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("old")); err != nil {
		t.Fatalf("initial WriteAtomic() error = %v", err)
	}
	alias := filepath.Join(t.TempDir(), "old.alias")
	if err := os.Link(layout.BackendStateFile(), alias); err != nil {
		t.Skipf("hard-link fixture unavailable: %v", err)
	}
	before := identityForPath(t, files, layout.BackendStateFile())
	beforeBytes, err := os.ReadFile(layout.BackendStateFile())
	if err != nil {
		t.Fatalf("read old destination error = %v", err)
	}
	result, err := files.WriteAtomic(t.Context(), StateBackend, []byte("new"))
	if err == nil || result.MutationApplied {
		t.Fatalf("WriteAtomic() = %#v, %v, want hard-link pre-commit failure", result, err)
	}
	after := identityForPath(t, files, layout.BackendStateFile())
	afterBytes, readErr := os.ReadFile(layout.BackendStateFile())
	if readErr != nil || before.fileID != after.fileID || !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatalf("foreign object changed: bytes %q/%v, identity %#v -> %#v", afterBytes, readErr, before, after)
	}
}

func TestStateFiles_ConcurrentWriterCannotTreatGapAsAbsent(t *testing.T) {
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
	if _, err := first.WriteAtomic(t.Context(), StateBackend, []byte("old")); err != nil {
		t.Fatalf("initial WriteAtomic() error = %v", err)
	}
	renameState := first.api.renameState
	gapEntered := make(chan struct{})
	releaseGap := make(chan struct{})
	first.api.renameState = func(source windows.Handle, parent windows.Handle, name string, flags uint32) error {
		sourcePath, pathErr := first.api.finalPath(source)
		err := renameState(source, parent, name, flags)
		if err == nil && pathErr == nil &&
			filepath.Base(sourcePath) == filepath.Base(layout.BackendStateFile()) &&
			strings.Contains(name, ".backup.") {
			close(gapEntered)
			select {
			case <-releaseGap:
			case <-t.Context().Done():
				return t.Context().Err()
			}
		}
		return err
	}
	firstDone := make(chan error, 1)
	go func() {
		_, writeErr := first.WriteAtomic(t.Context(), StateBackend, []byte("first"))
		firstDone <- writeErr
	}()
	waitStateTestSignal(t, gapEntered, "first writer raw gap")
	secondDone := make(chan error, 1)
	go func() {
		_, writeErr := second.WriteAtomic(t.Context(), StateBackend, []byte("second"))
		secondDone <- writeErr
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("second writer crossed exclusive guard early: %v", err)
	default:
	}
	close(releaseGap)
	if err := waitStateTestResult(t, firstDone, "first writer completion"); err != nil {
		t.Fatalf("first WriteAtomic() error = %v", err)
	}
	if err := waitStateTestResult(t, secondDone, "second writer completion"); err != nil {
		t.Fatalf("second WriteAtomic() error = %v", err)
	}
	snapshot, err := second.Read(t.Context(), StateBackend, MaxStateFileBytes)
	if err != nil || !bytes.Equal(snapshot.Bytes(), []byte("second")) {
		t.Fatalf("final Read() = %q, %v, want second", snapshot.Bytes(), err)
	}
}

func TestStateFiles_FinalizeDeletesBackupBeforeIntent(t *testing.T) {
	files, _ := newStateFilesFixture(t)

	probeAnchorSpec := ntCreateSpec{
		desiredAccess: windows.FILE_READ_DATA |
			windows.FILE_WRITE_DATA |
			windows.FILE_READ_ATTRIBUTES |
			windows.DELETE |
			windows.SYNCHRONIZE,
		shareAccess:       windows.FILE_SHARE_READ | windows.FILE_SHARE_DELETE,
		createDisposition: ntFileCreate,
		createOptions: ntFileOpenReparsePoint |
			ntFileNonDirectoryFile |
			ntFileSynchronousNonalert |
			ntFileDeleteOnClose,
	}
	existingAnchorSpec := openSpec{
		access: windows.FILE_READ_DATA |
			windows.FILE_READ_ATTRIBUTES |
			windows.DELETE |
			windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_DELETE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}
	createAnchorSpec := existingAnchorSpec
	createAnchorSpec.access |= windows.FILE_WRITE_DATA
	createAnchorSpec.creation = windows.CREATE_NEW
	unlinkSpec := openSpec{
		access:    windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}

	type unlinkHandle struct {
		leaf   string
		anchor windows.Handle
	}
	var mu sync.Mutex
	events := make([]string, 0, 15)
	anchorsByID := make(map[[16]byte]windows.Handle)
	anchorLeaf := make(map[windows.Handle]string)
	unlinkHandles := make(map[windows.Handle]unlinkHandle)
	unlinkClosed := make(map[string]bool)
	absentProved := make(map[string]bool)
	seenProbeAnchor := false
	seenExistingAnchor := false
	seenCreateAnchor := false

	recordAnchor := func(t *testing.T, handle windows.Handle) {
		t.Helper()
		identity, err := files.api.identity(handle)
		if err != nil {
			t.Fatalf("anchor identity error = %v", err)
		}
		mu.Lock()
		anchorsByID[identity.fileID] = handle
		mu.Unlock()
	}

	ntCreateRelative := files.api.ntCreateRelative
	files.api.ntCreateRelative = func(
		parent windows.Handle,
		name string,
		spec ntCreateSpec,
	) (windows.Handle, error) {
		handle, err := ntCreateRelative(parent, name, spec)
		if err == nil && strings.Contains(name, ".probe.") {
			if spec != probeAnchorSpec {
				t.Fatalf("probe anchor spec = %#v, want %#v", spec, probeAnchorSpec)
			}
			mu.Lock()
			seenProbeAnchor = true
			mu.Unlock()
			recordAnchor(t, handle)
		}
		return handle, err
	}

	openRelative := files.api.openRelative
	files.api.openRelative = func(
		parent windows.Handle,
		name string,
		spec openSpec,
	) (windows.Handle, error) {
		handle, err := openRelative(parent, name, spec)
		switch {
		case spec.access&windows.DELETE != 0 &&
			spec.access&windows.FILE_READ_DATA != 0 &&
			spec.creation == windows.OPEN_EXISTING:
			if spec != existingAnchorSpec {
				t.Fatalf("existing identity anchor spec for %q = %#v, want %#v", name, spec, existingAnchorSpec)
			}
			if err == nil {
				mu.Lock()
				seenExistingAnchor = true
				mu.Unlock()
				recordAnchor(t, handle)
			}
		case spec.access&windows.DELETE != 0 &&
			spec.access&windows.FILE_WRITE_DATA != 0 &&
			spec.creation == windows.CREATE_NEW:
			if spec != createAnchorSpec {
				t.Fatalf("created identity anchor spec for %q = %#v, want %#v", name, spec, createAnchorSpec)
			}
			if err == nil {
				mu.Lock()
				seenCreateAnchor = true
				mu.Unlock()
				recordAnchor(t, handle)
			}
		case spec == unlinkSpec:
			if err == nil {
				identity, identityErr := files.api.identity(handle)
				if identityErr != nil {
					t.Fatalf("unlink handle identity for %q error = %v", name, identityErr)
				}
				mu.Lock()
				anchor, ok := anchorsByID[identity.fileID]
				if !ok {
					mu.Unlock()
					t.Fatalf("unlink handle %q file ID %x has no matching identity anchor", name, identity.fileID)
				}
				anchorLeaf[anchor] = name
				unlinkHandles[handle] = unlinkHandle{leaf: name, anchor: anchor}
				mu.Unlock()
			}
		case spec == stateAbsenceProbeSpec() && err != nil && isWindowsNotFound(err):
			mu.Lock()
			if unlinkClosed[name] {
				events = append(events, "absent:"+stateUnlinkTestRole(name))
				absentProved[name] = true
			}
			mu.Unlock()
		}
		return handle, err
	}

	setDisposition := files.api.setStateDisposition
	files.api.setStateDisposition = func(handle windows.Handle, spec stateDispositionSpec) error {
		if spec != statePOSIXDispositionSpec() {
			t.Fatalf("state disposition spec = %#v, want %#v", spec, statePOSIXDispositionSpec())
		}
		mu.Lock()
		unlink, ok := unlinkHandles[handle]
		if !ok {
			mu.Unlock()
			t.Fatalf("disposition handle %#x is not an independently opened U", handle)
		}
		events = append(events, "disposition:"+stateUnlinkTestRole(unlink.leaf))
		mu.Unlock()
		return setDisposition(handle, spec)
	}

	closeHandle := files.api.closeHandle
	files.api.closeHandle = func(handle windows.Handle) error {
		err := closeHandle(handle)
		mu.Lock()
		if unlink, ok := unlinkHandles[handle]; ok && err == nil {
			events = append(events, "u-close:"+stateUnlinkTestRole(unlink.leaf))
			unlinkClosed[unlink.leaf] = true
			delete(unlinkHandles, handle)
		}
		mu.Unlock()
		return err
	}

	identity := files.api.identity
	files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
		got, err := identity(handle)
		mu.Lock()
		if leaf, ok := anchorLeaf[handle]; ok && absentProved[leaf] {
			events = append(events, "anchor-identity:"+stateUnlinkTestRole(leaf))
		}
		mu.Unlock()
		return got, err
	}

	readFile := files.api.readFile
	files.api.readFile = func(handle windows.Handle, buffer []byte) (int, error) {
		n, err := readFile(handle, buffer)
		mu.Lock()
		if leaf, ok := anchorLeaf[handle]; ok && absentProved[leaf] {
			events = append(events, "anchor-read:"+stateUnlinkTestRole(leaf))
		}
		mu.Unlock()
		return n, err
	}

	if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("old")); err != nil {
		t.Fatalf("initial WriteAtomic() error = %v", err)
	}
	if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("new")); err != nil {
		t.Fatalf("replacement WriteAtomic() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !seenProbeAnchor || !seenExistingAnchor || !seenCreateAnchor {
		t.Fatalf(
			"identity anchor coverage = probe:%t existing:%t create:%t, want all true",
			seenProbeAnchor,
			seenExistingAnchor,
			seenCreateAnchor,
		)
	}
	for _, role := range []string{"probe", "backup", "intent"} {
		want := []string{
			"disposition:" + role,
			"u-close:" + role,
			"absent:" + role,
			"anchor-identity:" + role,
			"anchor-read:" + role,
		}
		position := -1
		for _, event := range want {
			next := slicesIndexAfter(events, event, position+1)
			if next < 0 {
				t.Fatalf("unlink events = %v, missing ordered event %q for %s", events, event, role)
			}
			position = next
		}
	}
	backupAbsent := slicesIndexAfter(events, "absent:backup", 0)
	intentDisposition := slicesIndexAfter(events, "disposition:intent", 0)
	if backupAbsent < 0 || intentDisposition < 0 || backupAbsent >= intentDisposition {
		t.Fatalf("unlink events = %v, want backup absent before intent disposition", events)
	}
}

func stateUnlinkTestRole(leaf string) string {
	switch {
	case strings.Contains(leaf, ".probe."):
		return "probe"
	case strings.Contains(leaf, ".backup."):
		return "backup"
	case leaf == stateIntentLeaf(StateBackend):
		return "intent"
	default:
		return leaf
	}
}

func slicesIndexAfter(values []string, want string, start int) int {
	for index := start; index < len(values); index++ {
		if values[index] == want {
			return index
		}
	}
	return -1
}

func TestStateFiles_PostCommitFailureReportsAppliedAndRecovery(t *testing.T) {
	files, layout := newStateFilesFixture(t)
	if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("old")); err != nil {
		t.Fatalf("initial WriteAtomic() error = %v", err)
	}
	injected := errors.New("injected post-commit disposition failure")
	setDisposition := files.api.setStateDisposition
	files.api.setStateDisposition = func(handle windows.Handle, spec stateDispositionSpec) error {
		path, err := files.api.finalPath(handle)
		if err == nil && strings.Contains(filepath.Base(path), ".backup.") {
			return injected
		}
		return setDisposition(handle, spec)
	}
	result, err := files.WriteAtomic(t.Context(), StateBackend, []byte("new"))
	var writeErr *StateWriteError
	if result != (WriteAtomicResult{MutationApplied: true, RecoveryRequired: true}) ||
		!errors.As(err, &writeErr) ||
		writeErr.Phase != StateWritePhaseFinalize ||
		!errors.Is(err, injected) {
		t.Fatalf("WriteAtomic() = %#v, %v, want applied/recovery finalize error", result, err)
	}
	got, readErr := os.ReadFile(layout.BackendStateFile())
	if readErr != nil || !bytes.Equal(got, []byte("new")) {
		t.Fatalf("destination after post-commit failure = %q, %v, want new", got, readErr)
	}
}

func TestStateFiles_SourceHardLinkInjectionFailsClosed(t *testing.T) {
	files, layout := newStateFilesFixture(t)
	if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("old")); err != nil {
		t.Fatalf("initial WriteAtomic() error = %v", err)
	}
	alias := filepath.Join(t.TempDir(), "backend.alias")
	if err := os.Link(layout.BackendStateFile(), alias); err != nil {
		t.Skipf("hard-link fixture unavailable: %v", err)
	}
	result, err := files.WriteAtomic(t.Context(), StateBackend, []byte("new"))
	if err == nil || result.MutationApplied || !errors.Is(err, ErrUnsafeHardLink) {
		t.Fatalf("WriteAtomic() = %#v, %v, want hard-link failure before commit", result, err)
	}
	got, readErr := os.ReadFile(layout.BackendStateFile())
	if readErr != nil || !bytes.Equal(got, []byte("old")) {
		t.Fatalf("destination after hard-link rejection = %q, %v, want old", got, readErr)
	}
}

func TestStateFiles_WriteCancellationAfterRenameKeepsApplied(t *testing.T) {
	files, layout := newStateFilesFixture(t)
	if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("old")); err != nil {
		t.Fatalf("initial WriteAtomic() error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	renameState := files.api.renameState
	files.api.renameState = func(source windows.Handle, parent windows.Handle, name string, flags uint32) error {
		sourcePath, pathErr := files.api.finalPath(source)
		err := renameState(source, parent, name, flags)
		if err == nil && pathErr == nil &&
			strings.Contains(filepath.Base(sourcePath), ".temp.") &&
			name == filepath.Base(layout.BackendStateFile()) {
			cancel()
		}
		return err
	}
	result, err := files.WriteAtomic(ctx, StateBackend, []byte("new"))
	if err != nil || result != (WriteAtomicResult{MutationApplied: true}) {
		t.Fatalf("WriteAtomic() = %#v, %v, want applied clean success", result, err)
	}
	got, readErr := os.ReadFile(layout.BackendStateFile())
	if readErr != nil || !bytes.Equal(got, []byte("new")) {
		t.Fatalf("destination after commit cancellation = %q, %v, want new", got, readErr)
	}
}

func TestStateFiles_POSIXUnlinkCapabilityAndExactFlags(t *testing.T) {
	files, layout := newStateFilesFixture(t)
	snapshot := writeAndReadStateSnapshot(t, files, StateBackend, []byte("payload"))
	openRelative := files.api.openRelative
	setDisposition := files.api.setStateDisposition
	closeHandle := files.api.closeHandle
	identity := files.api.identity
	readFile := files.api.readFile
	var unlinkOpens []openSpec
	var dispositions []stateDispositionSpec
	var events []string
	var anchorHandle windows.Handle
	var unlinkHandle windows.Handle
	var anchorID [16]byte
	var unlinkClosed bool
	files.api.openRelative = func(
		parent windows.Handle,
		name string,
		spec openSpec,
	) (windows.Handle, error) {
		handle, err := openRelative(parent, name, spec)
		if name != filepath.Base(layout.BackendStateFile()) {
			return handle, err
		}
		switch {
		case err == nil && spec == stateMutationOpenSpec():
			anchorHandle = handle
			value, identityErr := identity(handle)
			if identityErr != nil {
				t.Fatalf("anchor identity error = %v", identityErr)
			}
			anchorID = value.fileID
		case err == nil && spec == stateUnlinkOpenSpec():
			unlinkHandle = handle
			unlinkOpens = append(unlinkOpens, spec)
			value, identityErr := identity(handle)
			if identityErr != nil {
				t.Fatalf("unlink identity error = %v", identityErr)
			}
			if value.fileID != anchorID {
				t.Fatalf("unlink file ID = %x, want anchor %x", value.fileID, anchorID)
			}
		case spec == stateAbsenceProbeSpec():
			if !unlinkClosed {
				t.Fatal("relative absence check happened before U close")
			}
			if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
				t.Fatalf("relative absence error = %v, want file not found", err)
			}
			events = append(events, "absent")
		}
		return handle, err
	}
	files.api.setStateDisposition = func(
		handle windows.Handle,
		spec stateDispositionSpec,
	) error {
		dispositions = append(dispositions, spec)
		if handle == unlinkHandle {
			events = append(events, "disposition")
		}
		return setDisposition(handle, spec)
	}
	files.api.closeHandle = func(handle windows.Handle) error {
		err := closeHandle(handle)
		if handle == unlinkHandle && err == nil {
			unlinkClosed = true
			events = append(events, "u-close")
		}
		return err
	}
	files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
		value, err := identity(handle)
		if err == nil && handle == anchorHandle && unlinkClosed {
			if value.fileID != anchorID {
				t.Fatalf("anchor file ID after unlink = %x, want %x", value.fileID, anchorID)
			}
			events = append(events, "anchor-identity")
		}
		return value, err
	}
	files.api.readFile = func(handle windows.Handle, buffer []byte) (int, error) {
		n, err := readFile(handle, buffer)
		if handle == anchorHandle && unlinkClosed {
			events = append(events, "anchor-read")
		}
		return n, err
	}

	result, err := files.RemoveTransactionIfUnchanged(t.Context(), snapshot)
	if err != nil || result != (StateRemoveResult{MutationApplied: true}) {
		t.Fatalf("RemoveTransactionIfUnchanged() = %#v, %v, want applied success", result, err)
	}
	if len(unlinkOpens) != 1 {
		t.Fatalf("actual unlink opens = %d, want 1", len(unlinkOpens))
	}
	wantOpen := stateUnlinkOpenSpec()
	if unlinkOpens[0] != wantOpen {
		t.Fatalf("actual unlink open = %#v, want %#v", unlinkOpens[0], wantOpen)
	}
	if len(dispositions) != 1 || dispositions[0] != statePOSIXDispositionSpec() {
		t.Fatalf("actual dispositions = %#v, want exact POSIX disposition", dispositions)
	}
	requiredOrder := []string{
		"disposition",
		"u-close",
		"absent",
		"anchor-identity",
		"anchor-read",
	}
	previous := -1
	for _, event := range requiredOrder {
		index := slicesIndexAfter(events, event, previous+1)
		if index < 0 {
			t.Fatalf("events = %v, missing %q after index %d", events, event, previous)
		}
		previous = index
	}
	if _, err := os.Stat(layout.BackendStateFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination after remove error = %v, want not-exist", err)
	}

	t.Run("cached probe actual disposition failure is known not applied", func(t *testing.T) {
		failingFiles, failingLayout := newStateFilesFixture(t)
		failingSnapshot := writeAndReadStateSnapshot(
			t,
			failingFiles,
			StateBackend,
			[]byte("payload"),
		)
		injected := errors.New("injected disposition failure")
		failingFiles.api.setStateDisposition = func(
			windows.Handle,
			stateDispositionSpec,
		) error {
			return injected
		}
		result, err := failingFiles.RemoveTransactionIfUnchanged(t.Context(), failingSnapshot)
		if result != (StateRemoveResult{}) || !errors.Is(err, injected) {
			t.Fatalf("known-not-applied remove = %#v, %v", result, err)
		}
		got, readErr := os.ReadFile(failingLayout.BackendStateFile())
		if readErr != nil || !bytes.Equal(got, []byte("payload")) {
			t.Fatalf("destination after failed disposition = %q, %v", got, readErr)
		}
	})

	t.Run("unsupported probe leaves destination stable", func(t *testing.T) {
		unsupportedFiles, unsupportedLayout := newStateFilesFixture(t)
		unsupportedSnapshot := writeAndReadStateSnapshot(
			t,
			unsupportedFiles,
			StateBackend,
			[]byte("payload"),
		)
		unsupportedFiles.probePassed[StateBackend] = false
		unsupportedFiles.api.setStateDisposition = func(
			windows.Handle,
			stateDispositionSpec,
		) error {
			return windows.ERROR_INVALID_PARAMETER
		}
		result, err := unsupportedFiles.RemoveTransactionIfUnchanged(
			t.Context(),
			unsupportedSnapshot,
		)
		if result != (StateRemoveResult{}) ||
			!errors.Is(err, ErrPOSIXUnlinkUnsupported) {
			t.Fatalf("unsupported remove = %#v, %v", result, err)
		}
		got, readErr := os.ReadFile(unsupportedLayout.BackendStateFile())
		if readErr != nil || !bytes.Equal(got, []byte("payload")) {
			t.Fatalf("destination after unsupported probe = %q, %v", got, readErr)
		}
	})

	t.Run("U close ambiguity requires recovery", func(t *testing.T) {
		ambiguousFiles, _ := newStateFilesFixture(t)
		ambiguousSnapshot := writeAndReadStateSnapshot(
			t,
			ambiguousFiles,
			StateBackend,
			[]byte("payload"),
		)
		openRelative := ambiguousFiles.api.openRelative
		closeHandle := ambiguousFiles.api.closeHandle
		var unlinkHandle windows.Handle
		ambiguousFiles.api.openRelative = func(
			parent windows.Handle,
			name string,
			spec openSpec,
		) (windows.Handle, error) {
			handle, err := openRelative(parent, name, spec)
			if err == nil && spec == stateUnlinkOpenSpec() {
				unlinkHandle = handle
			}
			return handle, err
		}
		injected := errors.New("injected U close ambiguity")
		ambiguousFiles.api.closeHandle = func(handle windows.Handle) error {
			err := closeHandle(handle)
			if handle == unlinkHandle {
				return errors.Join(injected, err)
			}
			return err
		}
		result, err := ambiguousFiles.RemoveTransactionIfUnchanged(
			t.Context(),
			ambiguousSnapshot,
		)
		if result != (StateRemoveResult{RecoveryRequired: true}) ||
			!errors.Is(err, injected) {
			t.Fatalf("ambiguous U close remove = %#v, %v", result, err)
		}
	})
}

func TestStateFiles_ConditionalRemoveResultMatrix(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		files, _ := newStateFilesFixture(t)
		snapshot := writeAndReadStateSnapshot(t, files, StateBackend, []byte("payload"))
		result, err := files.RemoveTransactionIfUnchanged(t.Context(), snapshot)
		if err != nil || result != (StateRemoveResult{MutationApplied: true}) {
			t.Fatalf("RemoveTransactionIfUnchanged() = %#v, %v, want applied success", result, err)
		}
	})

	t.Run("clean mismatch", func(t *testing.T) {
		files, _ := newStateFilesFixture(t)
		snapshot := writeAndReadStateSnapshot(t, files, StateBackend, []byte("old"))
		if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("new")); err != nil {
			t.Fatalf("replacement WriteAtomic() error = %v", err)
		}
		result, err := files.RemoveTransactionIfUnchanged(t.Context(), snapshot)
		var removeErr *StateRemoveError
		if result != (StateRemoveResult{}) || !errors.Is(err, ErrIdentityChanged) ||
			errors.As(err, &removeErr) {
			t.Fatalf("mismatch remove = %#v, %v, want bare identity mismatch", result, err)
		}
	})

	t.Run("missing then guard close failure", func(t *testing.T) {
		files, layout := newStateFilesFixture(t)
		snapshot := writeAndReadStateSnapshot(t, files, StateBackend, []byte("payload"))
		if err := os.Remove(layout.BackendStateFile()); err != nil {
			t.Fatalf("os.Remove(destination) error = %v", err)
		}
		injectStateGuardCloseFailure(files, StateBackend, errors.New("injected guard close failure"))
		result, err := files.RemoveTransactionIfUnchanged(t.Context(), snapshot)
		if result != (StateRemoveResult{RecoveryRequired: true}) ||
			err == nil {
			t.Fatalf("missing remove with close failure = %#v, %v", result, err)
		}
	})

	t.Run("mismatch then guard close failure", func(t *testing.T) {
		files, _ := newStateFilesFixture(t)
		snapshot := writeAndReadStateSnapshot(t, files, StateBackend, []byte("old"))
		if _, err := files.WriteAtomic(t.Context(), StateBackend, []byte("new")); err != nil {
			t.Fatalf("replacement WriteAtomic() error = %v", err)
		}
		injected := errors.New("injected guard close failure")
		injectStateGuardCloseFailure(files, StateBackend, injected)
		result, err := files.RemoveTransactionIfUnchanged(t.Context(), snapshot)
		var removeErr *StateRemoveError
		if result != (StateRemoveResult{RecoveryRequired: true}) ||
			!errors.Is(err, ErrIdentityChanged) ||
			!errors.Is(err, injected) ||
			!errors.As(err, &removeErr) {
			t.Fatalf("mismatch remove with close failure = %#v, %v", result, err)
		}
	})

	t.Run("commit then guard close failure", func(t *testing.T) {
		files, _ := newStateFilesFixture(t)
		snapshot := writeAndReadStateSnapshot(t, files, StateBackend, []byte("payload"))
		injected := errors.New("injected guard close failure")
		injectStateGuardCloseFailure(files, StateBackend, injected)
		result, err := files.RemoveTransactionIfUnchanged(t.Context(), snapshot)
		if result != (StateRemoveResult{
			MutationApplied:  true,
			RecoveryRequired: true,
		}) || !errors.Is(err, injected) {
			t.Fatalf("committed remove with close failure = %#v, %v", result, err)
		}
	})
}

func TestStateFiles_ConditionalRemoveRecoversOrRefusesIntent(t *testing.T) {
	t.Run("snapshot from backup is recovered before remove", func(t *testing.T) {
		files, layout := newStateFilesFixture(t)
		fixture := newSealedStateFixture(
			t,
			files,
			layout,
			StateBackend,
			[]byte("old"),
			[]byte("new"),
		)
		fixture.installOldAtBackup(t)
		fixture.installNewAtTemp(t)
		fixture.publishIntent(t)
		snapshot, err := files.Read(t.Context(), StateBackend, MaxStateFileBytes)
		if err != nil {
			t.Fatalf("Read() from backup error = %v", err)
		}
		result, err := files.RemoveTransactionIfUnchanged(t.Context(), snapshot)
		if err != nil || result != (StateRemoveResult{MutationApplied: true}) {
			t.Fatalf("recovered remove = %#v, %v, want applied success", result, err)
		}
	})

	t.Run("unsealed intent is refused", func(t *testing.T) {
		files, layout := newStateFilesFixture(t)
		snapshot := writeAndReadStateSnapshot(t, files, StateBackend, []byte("old"))
		if err := os.WriteFile(
			filepath.Join(layout.StateDir(), stateIntentLeaf(StateBackend)),
			[]byte("foreign"),
			0o600,
		); err != nil {
			t.Fatalf("os.WriteFile(foreign intent) error = %v", err)
		}
		result, err := files.RemoveTransactionIfUnchanged(t.Context(), snapshot)
		if !result.RecoveryRequired || result.MutationApplied || err == nil {
			t.Fatalf("foreign intent remove = %#v, %v, want recovery failure", result, err)
		}
	})
}

func TestStateFiles_MissingRemoveDoesNotRequirePOSIXUnlink(t *testing.T) {
	tests := []struct {
		name    string
		orphans bool
	}{
		{name: "clean missing"},
		{name: "missing with orphans", orphans: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files, layout := newStateFilesFixture(t)
			snapshot := writeAndReadStateSnapshot(t, files, StateBackend, []byte("payload"))
			if err := os.Remove(layout.BackendStateFile()); err != nil {
				t.Fatalf("os.Remove(destination) error = %v", err)
			}
			var orphanPaths []string
			if test.orphans {
				orphanPaths = []string{
					filepath.Join(layout.StateDir(), ".backend.temp-orphan"),
					filepath.Join(layout.StateDir(), ".backend.intent-orphan"),
				}
				for _, path := range orphanPaths {
					if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
						t.Fatalf("os.WriteFile(%q) error = %v", path, err)
					}
				}
			}
			var dispositions atomic.Int32
			files.api.setStateDisposition = func(windows.Handle, stateDispositionSpec) error {
				dispositions.Add(1)
				return ErrPOSIXUnlinkUnsupported
			}
			result, err := files.RemoveTransactionIfUnchanged(t.Context(), snapshot)
			if err != nil || result != (StateRemoveResult{}) {
				t.Fatalf("missing remove = %#v, %v, want idempotent success", result, err)
			}
			if got := dispositions.Load(); got != 0 {
				t.Fatalf("disposition calls = %d, want 0", got)
			}
			for _, path := range orphanPaths {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("orphan %q changed: %v", path, err)
				}
			}
		})
	}
}

func TestStateFiles_ConcurrentKindsSerializeProbeCache(t *testing.T) {
	files, _ := newStateFilesFixture(t)
	backend := writeAndReadStateSnapshot(t, files, StateBackend, []byte("backend"))
	if _, err := files.WriteAtomic(t.Context(), StateMutation, []byte("mutation")); err != nil {
		t.Fatalf("mutation WriteAtomic() error = %v", err)
	}
	setDisposition := files.api.setStateDisposition
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	files.api.setStateDisposition = func(
		handle windows.Handle,
		spec stateDispositionSpec,
	) error {
		once.Do(func() {
			close(entered)
			<-release
		})
		return setDisposition(handle, spec)
	}
	removeDone := make(chan error, 1)
	go func() {
		_, err := files.RemoveTransactionIfUnchanged(t.Context(), backend)
		removeDone <- err
	}()
	waitStateTestSignal(t, entered, "backend remove disposition")
	writeDone := make(chan error, 1)
	go func() {
		_, err := files.WriteAtomic(t.Context(), StateMutation, []byte("mutation-next"))
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		t.Fatalf("different-kind write escaped StateFiles lock: %v", err)
	default:
	}
	close(release)
	if err := waitStateTestResult(t, removeDone, "serialized remove"); err != nil {
		t.Fatalf("RemoveTransactionIfUnchanged() error = %v", err)
	}
	if err := waitStateTestResult(t, writeDone, "serialized write"); err != nil {
		t.Fatalf("WriteAtomic() error = %v", err)
	}
}

func TestStateFiles_WriteAndRemoveRejectAfterClose(t *testing.T) {
	files, _ := newStateFilesFixture(t)
	snapshot := writeAndReadStateSnapshot(t, files, StateBackend, []byte("payload"))
	if err := files.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	var ioCalls atomic.Int32
	files.api.ntCreateRelative = func(windows.Handle, string, ntCreateSpec) (windows.Handle, error) {
		ioCalls.Add(1)
		return windows.InvalidHandle, errors.New("unexpected I/O")
	}
	writeResult, writeErr := files.WriteAtomic(t.Context(), StateBackend, []byte("next"))
	removeResult, removeErr := files.RemoveTransactionIfUnchanged(t.Context(), snapshot)
	if writeResult != (WriteAtomicResult{}) || !errors.Is(writeErr, ErrClosed) {
		t.Fatalf("WriteAtomic() after Close = %#v, %v", writeResult, writeErr)
	}
	if removeResult != (StateRemoveResult{}) || !errors.Is(removeErr, ErrClosed) {
		t.Fatalf("RemoveTransactionIfUnchanged() after Close = %#v, %v", removeResult, removeErr)
	}
	if got := ioCalls.Load(); got != 0 {
		t.Fatalf("I/O calls after Close = %d, want 0", got)
	}
}

func TestStateFiles_RemoveCancellationAfterDispositionKeepsApplied(t *testing.T) {
	files, layout := newStateFilesFixture(t)
	snapshot := writeAndReadStateSnapshot(t, files, StateBackend, []byte("payload"))
	ctx, cancel := context.WithCancel(t.Context())
	setDisposition := files.api.setStateDisposition
	files.api.setStateDisposition = func(
		handle windows.Handle,
		spec stateDispositionSpec,
	) error {
		err := setDisposition(handle, spec)
		if err == nil {
			cancel()
		}
		return err
	}
	result, err := files.RemoveTransactionIfUnchanged(ctx, snapshot)
	if err != nil || result != (StateRemoveResult{MutationApplied: true}) {
		t.Fatalf("remove after cancellation = %#v, %v, want applied success", result, err)
	}
	if _, err := os.Stat(layout.BackendStateFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination after cancellation error = %v, want not-exist", err)
	}
}

func TestStateFiles_WriteAndRemoveContextsRejectedBeforeIO(t *testing.T) {
	files, _ := newStateFilesFixture(t)
	snapshot := writeAndReadStateSnapshot(t, files, StateBackend, []byte("payload"))
	foreignFiles, _ := newStateFilesFixture(t)
	foreignSnapshot := writeAndReadStateSnapshot(
		t,
		foreignFiles,
		StateBackend,
		[]byte("foreign"),
	)
	environmentSnapshot := writeAndReadStateSnapshot(
		t,
		files,
		StateEnvironment,
		[]byte("environment"),
	)
	var ioCalls atomic.Int32
	files.api.ntCreateRelative = func(windows.Handle, string, ntCreateSpec) (windows.Handle, error) {
		ioCalls.Add(1)
		return windows.InvalidHandle, errors.New("unexpected I/O")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	writeContexts := []context.Context{nil, canceled}
	for _, ctx := range writeContexts {
		if result, err := files.WriteAtomic(ctx, StateBackend, []byte("next")); err == nil ||
			result != (WriteAtomicResult{}) {
			t.Fatalf("WriteAtomic(%v) = %#v, %v, want pre-I/O error", ctx, result, err)
		}
	}
	removeContexts := []context.Context{nil, canceled}
	for _, ctx := range removeContexts {
		if result, err := files.RemoveTransactionIfUnchanged(ctx, snapshot); err == nil ||
			result != (StateRemoveResult{}) {
			t.Fatalf("RemoveTransactionIfUnchanged(%v) = %#v, %v, want pre-I/O error", ctx, result, err)
		}
	}
	tampered := snapshot
	tampered.bytes = append([]byte(nil), snapshot.bytes...)
	tampered.bytes[0] ^= 0xff
	invalidSnapshots := []StateFileSnapshot{
		{},
		foreignSnapshot,
		environmentSnapshot,
		tampered,
	}
	for _, invalid := range invalidSnapshots {
		result, err := files.RemoveTransactionIfUnchanged(t.Context(), invalid)
		if result != (StateRemoveResult{}) || !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("RemoveTransactionIfUnchanged(invalid) = %#v, %v", result, err)
		}
	}
	if got := ioCalls.Load(); got != 0 {
		t.Fatalf("I/O calls = %d, want 0", got)
	}
}

func TestWindows_StateGuardReleasedAtEveryCrashCutpoint(t *testing.T) {
	if point := os.Getenv("AUTO_MAS_STATE_CRASH_POINT"); point != "" {
		runStateRemoveCrashChild(t, point)
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	tests := []struct {
		point       string
		wantMissing bool
	}{
		{point: "before-disposition"},
		{point: "after-disposition", wantMissing: true},
		{point: "after-anchor-verification", wantMissing: true},
	}
	for _, test := range tests {
		t.Run(test.point, func(t *testing.T) {
			layout := newStateFilesTestLayout(t)
			command := exec.Command(
				executable,
				"-test.run=^TestWindows_StateGuardReleasedAtEveryCrashCutpoint$",
			)
			command.Env = append(
				os.Environ(),
				"AUTO_MAS_STATE_CRASH_POINT="+test.point,
				"AUTO_MAS_STATE_CRASH_ROOT="+layout.AppRoot(),
			)
			output, err := command.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != stateCrashExitCode {
				t.Fatalf("crash child error = %v, output = %s", err, output)
			}
			files, err := NewStateFiles(t.Context(), layout)
			if err != nil {
				t.Fatalf("NewStateFiles() after crash error = %v", err)
			}
			guard, err := files.acquireStateGuard(t.Context(), StateBackend, stateGuardExclusive)
			if err != nil {
				closeErr := files.Close()
				t.Fatalf("exclusive guard after crash error = %v; Close() error = %v", err, closeErr)
			}
			if err := files.closeStateObject(&guard); err != nil {
				closeErr := files.Close()
				t.Fatalf("guard close after crash error = %v; Close() error = %v", err, closeErr)
			}
			_, statErr := os.Stat(layout.BackendStateFile())
			if test.wantMissing && !errors.Is(statErr, os.ErrNotExist) {
				closeErr := files.Close()
				t.Fatalf(
					"destination after %s error = %v, want missing; Close() error = %v",
					test.point,
					statErr,
					closeErr,
				)
			}
			if !test.wantMissing && statErr != nil {
				closeErr := files.Close()
				t.Fatalf(
					"destination after %s error = %v, want present; Close() error = %v",
					test.point,
					statErr,
					closeErr,
				)
			}
			if err := files.Close(); err != nil {
				t.Fatalf("Close() after crash error = %v", err)
			}
		})
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
	nonce            string
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

const stateCrashExitCode = 86

func writeAndReadStateSnapshot(
	t *testing.T,
	files *StateFiles,
	kind StateFileKind,
	payload []byte,
) StateFileSnapshot {
	t.Helper()
	if _, err := files.WriteAtomic(t.Context(), kind, payload); err != nil {
		t.Fatalf("WriteAtomic(%s) error = %v", kind, err)
	}
	snapshot, err := files.Read(t.Context(), kind, MaxStateFileBytes)
	if err != nil {
		t.Fatalf("Read(%s) error = %v", kind, err)
	}
	return snapshot
}

func injectStateGuardCloseFailure(files *StateFiles, kind StateFileKind, injectedErr error) {
	ntCreateRelative := files.api.ntCreateRelative
	var guardHandle windows.Handle
	files.api.ntCreateRelative = func(
		parent windows.Handle,
		name string,
		spec ntCreateSpec,
	) (windows.Handle, error) {
		handle, err := ntCreateRelative(parent, name, spec)
		if err == nil && name == stateGuardLeaf(kind) {
			guardHandle = handle
		}
		return handle, err
	}
	closeHandle := files.api.closeHandle
	var injected atomic.Bool
	files.api.closeHandle = func(handle windows.Handle) error {
		err := closeHandle(handle)
		if handle == guardHandle && injected.CompareAndSwap(false, true) {
			return errors.Join(injectedErr, err)
		}
		return err
	}
}

func runStateRemoveCrashChild(t *testing.T, point string) {
	t.Helper()
	root := os.Getenv("AUTO_MAS_STATE_CRASH_ROOT")
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	files, err := NewStateFiles(t.Context(), layout)
	if err != nil {
		t.Fatalf("NewStateFiles() error = %v", err)
	}
	snapshot := writeAndReadStateSnapshot(t, files, StateBackend, []byte("payload"))

	openRelative := files.api.openRelative
	closeHandle := files.api.closeHandle
	setDisposition := files.api.setStateDisposition
	identity := files.api.identity
	var anchorHandle windows.Handle
	var unlinkHandle windows.Handle
	var unlinkClosed atomic.Bool
	files.api.openRelative = func(
		parent windows.Handle,
		name string,
		spec openSpec,
	) (windows.Handle, error) {
		handle, openErr := openRelative(parent, name, spec)
		if openErr == nil && name == filepath.Base(layout.BackendStateFile()) {
			switch spec {
			case stateMutationOpenSpec():
				anchorHandle = handle
			case stateUnlinkOpenSpec():
				unlinkHandle = handle
			}
		}
		return handle, openErr
	}
	files.api.setStateDisposition = func(
		handle windows.Handle,
		spec stateDispositionSpec,
	) error {
		if handle == unlinkHandle && point == "before-disposition" {
			os.Exit(stateCrashExitCode)
		}
		err := setDisposition(handle, spec)
		if err == nil && handle == unlinkHandle && point == "after-disposition" {
			os.Exit(stateCrashExitCode)
		}
		return err
	}
	files.api.closeHandle = func(handle windows.Handle) error {
		err := closeHandle(handle)
		if handle == unlinkHandle && err == nil {
			unlinkClosed.Store(true)
		}
		return err
	}
	files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
		value, identityErr := identity(handle)
		if identityErr == nil &&
			handle == anchorHandle &&
			unlinkClosed.Load() &&
			point == "after-anchor-verification" {
			os.Exit(stateCrashExitCode)
		}
		return value, identityErr
	}
	_, err = files.RemoveTransactionIfUnchanged(t.Context(), snapshot)
	t.Fatalf("RemoveTransactionIfUnchanged() returned at crash point %q: %v", point, err)
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
		nonce:            "0123456789abcdef0123456789abcdef",
		oldPayload:       append([]byte(nil), oldPayload...),
		newPayload:       append([]byte(nil), newPayload...),
		foreignPayload:   []byte("foreign"),
		destinationPath:  stateDestinationPath(layout, kind),
		guardPath:        filepath.Join(layout.StateDir(), stateGuardLeaf(kind)),
		intentPath:       filepath.Join(layout.StateDir(), stateIntentLeaf(kind)),
		backupPath:       filepath.Join(layout.StateDir(), stateTransactionLeaf(kind, "backup", "0123456789abcdef0123456789abcdef")),
		tempPath:         filepath.Join(layout.StateDir(), stateTransactionLeaf(kind, "temp", "0123456789abcdef0123456789abcdef")),
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
		Nonce:           f.nonce,
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

func (f *sealedStateFixture) publishUncheckedIntentValue(t *testing.T, intent stateIntent) {
	t.Helper()
	body, err := json.Marshal(intent)
	if err != nil {
		t.Fatalf("marshal unchecked state intent error = %v", err)
	}
	envelope, err := json.Marshal(stateIntentEnvelope{
		Intent: intent,
		Seal:   sha256.Sum256(body),
	})
	if err != nil {
		t.Fatalf("marshal unchecked state intent envelope error = %v", err)
	}
	if err := os.WriteFile(f.intentPath, envelope, 0o600); err != nil {
		t.Fatalf("write unchecked intent error = %v", err)
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
	waitStateTestSignal(t, entered, "incompatible guard wait")
	select {
	case err := <-done:
		t.Fatalf("waiter acquired incompatible guard early: %v", err)
	default:
	}
	if err := holder.api.closeHandle(held.handle); err != nil {
		t.Fatalf("holder close error = %v", err)
	}
	close(release)
	if err := waitStateTestResult(t, done, "guard acquire after release"); err != nil {
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
				select {
				case <-release:
				case <-t.Context().Done():
					return windows.InvalidHandle, t.Context().Err()
				}
			}
			return openRelative(parent, name, spec)
		}
	case "read":
		readFile := files.api.readFile
		var once sync.Once
		files.api.readFile = func(handle windows.Handle, buffer []byte) (int, error) {
			once.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-t.Context().Done():
				}
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
				select {
				case <-release:
				case <-t.Context().Done():
				}
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
	waitStateTestSignal(t, entered, "read entered "+point)
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
	if err := waitStateTestResult(t, readDone, "Read completion at "+point); err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if err := waitStateTestResult(t, closeDone, "Close completion at "+point); err != nil {
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

func waitStateTestSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", description)
	case <-t.Context().Done():
		t.Fatalf("context ended while waiting for %s: %v", description, t.Context().Err())
	}
}

func waitStateTestResult[T any](t *testing.T, result <-chan T, description string) T {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case value := <-result:
		return value
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", description)
		var zero T
		return zero
	case <-t.Context().Done():
		t.Fatalf("context ended while waiting for %s: %v", description, t.Context().Err())
		var zero T
		return zero
	}
}

func (tContextWithoutFailure) Deadline() (time.Time, bool) { return time.Time{}, false }
func (tContextWithoutFailure) Done() <-chan struct{}       { return nil }
func (tContextWithoutFailure) Err() error                  { return nil }
func (tContextWithoutFailure) Value(any) any               { return nil }

var _ context.Context = tContextWithoutFailure{}
var _ = sync.Mutex{}
