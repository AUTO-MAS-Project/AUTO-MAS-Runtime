package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestNewRuntimeLogFiles_CreatesAndPinsLayoutDirectories(t *testing.T) {
	layout := newRuntimeLogTestLayout(t)
	files, err := NewRuntimeLogFiles(t.Context(), layout)
	if err != nil {
		t.Fatalf("NewRuntimeLogFiles() error = %v", err)
	}
	for _, path := range []string{layout.LogsDir(), layout.RuntimeLogDir()} {
		information, err := os.Stat(path)
		if err != nil || !information.IsDir() {
			t.Fatalf("os.Stat(%q) = %v, error = %v, want directory", path, information, err)
		}
	}
	assertRuntimeLogAncestorsPinned(t, layout)
	if err := files.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestNewRuntimeLogFiles_RejectsReparseAncestors(t *testing.T) {
	layout := newRuntimeLogTestLayout(t)
	external := t.TempDir()
	if err := os.Symlink(external, layout.LogsDir()); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	files, err := NewRuntimeLogFiles(t.Context(), layout)
	if files != nil {
		_ = files.Close()
		t.Fatal("NewRuntimeLogFiles() returned a capability through a reparse point")
	}
	assertFilesystemCode(t, err, protocol.CodeUnsafeReparsePoint)
}

func TestRuntimeLogFiles_OpenAppendUsesLayoutAndSingleLink(t *testing.T) {
	files, layout := newRuntimeLogFixture(t)
	date := time.Date(2026, 7, 29, 12, 0, 0, 0, time.Local)
	writer, err := files.OpenAppend(t.Context(), "sync", date)
	if err != nil {
		t.Fatalf("OpenAppend() error = %v", err)
	}
	wantPath, err := layout.RuntimeLogFile("sync", date)
	if err != nil {
		t.Fatalf("RuntimeLogFile() error = %v", err)
	}
	if writer.Path() != wantPath {
		t.Fatalf("Path() = %q, want %q", writer.Path(), wantPath)
	}
	if _, err := writer.Write([]byte("first\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}
	writer, err = files.OpenAppend(t.Context(), "sync", date)
	if err != nil {
		t.Fatalf("second OpenAppend() error = %v", err)
	}
	if _, err := writer.Write([]byte("second\n")); err != nil {
		t.Fatalf("second Write() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("second writer.Close() error = %v", err)
	}
	got, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if string(got) != "first\nsecond\n" {
		t.Fatalf("log contents = %q, want append contents", got)
	}
}

func TestRuntimeLogWriter_WriteRevalidatesHardLinkCount(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	writer, err := files.OpenAppend(t.Context(), "sync", time.Now())
	if err != nil {
		t.Fatalf("OpenAppend() error = %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	alias := writer.Path() + ".alias"
	if err := os.Link(writer.Path(), alias); err != nil {
		t.Skipf("hard-link fixture unavailable: %v", err)
	}
	if _, err := writer.Write([]byte("unsafe")); !errors.Is(err, ErrUnsafeHardLink) {
		t.Fatalf("Write() error = %v, want ErrUnsafeHardLink", err)
	}
}

func TestRuntimeLogFiles_CloseLeavesWriterPinsAlive(t *testing.T) {
	files, layout := newRuntimeLogFixture(t)
	writer, err := files.OpenAppend(t.Context(), "sync", time.Now())
	if err != nil {
		t.Fatalf("OpenAppend() error = %v", err)
	}
	if err := files.Close(); err != nil {
		t.Fatalf("files.Close() error = %v", err)
	}
	if _, err := writer.Write([]byte("after parent close")); err != nil {
		t.Fatalf("writer.Write() error = %v", err)
	}
	assertRuntimeLogAncestorsPinned(t, layout)
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}
	assertRuntimeLogAncestorsRenamable(t, layout)
}

func TestRuntimeLogWriter_CloseIsIdempotent(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	writer, err := files.OpenAppend(t.Context(), "sync", time.Now())
	if err != nil {
		t.Fatalf("OpenAppend() error = %v", err)
	}
	first := writer.Close()
	second := writer.Close()
	if !errors.Is(second, first) && second != first {
		t.Fatalf("second Close() error = %v, want cached %v", second, first)
	}
}

func TestRuntimeLogFiles_OpenAppendRejectsAfterClose(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	if err := files.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	writer, err := files.OpenAppend(t.Context(), "sync", time.Now())
	if writer != nil || !errors.Is(err, ErrClosed) {
		t.Fatalf("OpenAppend() = %#v, %v, want nil, ErrClosed", writer, err)
	}
}

func TestRuntimeLogFiles_CloseIsIdempotent(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	first := files.Close()
	second := files.Close()
	if !errors.Is(second, first) && second != first {
		t.Fatalf("second Close() error = %v, want cached %v", second, first)
	}
}

func TestRuntimeLogFiles_ContextRejectedBeforeIO(t *testing.T) {
	layout := newRuntimeLogTestLayout(t)
	api := newProductionPathAPI()
	openCalls := 0
	openPath := api.openPath
	api.openPath = func(path string, spec openSpec) (windows.Handle, error) { openCalls++; return openPath(path, spec) }
	if files, err := newRuntimeLogFilesWith(nil, layout, runtimeLogDependencies{api: api}); files != nil || !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("nil-context constructor = %#v, %v", files, err)
	}
	if openCalls != 0 {
		t.Fatalf("nil-context open calls = %d, want 0", openCalls)
	}
	files, _ := newRuntimeLogFixture(t)
	relativeCalls := 0
	openRelative := files.api.openRelative
	files.api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
		relativeCalls++
		return openRelative(parent, name, spec)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	writer, err := files.OpenAppend(ctx, "sync", time.Now())
	if writer != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel OpenAppend() = %#v, %v", writer, err)
	}
	if relativeCalls != 0 {
		t.Fatalf("pre-cancel relative-open calls = %d, want 0", relativeCalls)
	}
}

func TestRuntimeLogFiles_OpenAppendMatchesAccessMatrixAndParentIdentity(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	var got openSpec
	openRelative := files.api.openRelative
	files.api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
		got = spec
		return openRelative(parent, name, spec)
	}
	writer, err := files.OpenAppend(t.Context(), "sync", time.Now())
	if err != nil {
		t.Fatalf("OpenAppend() error = %v", err)
	}
	defer func() { _ = writer.Close() }()
	want := openSpec{access: windows.FILE_APPEND_DATA | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE, share: windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE, creation: windows.OPEN_ALWAYS, options: windows.FILE_FLAG_OPEN_REPARSE_POINT, directory: false}
	if got != want {
		t.Fatalf("append openSpec = %#v, want %#v", got, want)
	}
	if got.share&windows.FILE_SHARE_WRITE == 0 || got.share&windows.FILE_SHARE_DELETE != 0 {
		t.Fatalf("append share = %#x, want share-read/share-write without share-delete", got.share)
	}
}

func TestRuntimeLogWriter_WriteAndCloseUseStoredAPI(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	writeCalls := 0
	writeFile := files.api.writeFile
	files.api.writeFile = func(handle windows.Handle, payload []byte) (int, error) {
		writeCalls++
		return writeFile(handle, payload)
	}
	writer, err := files.OpenAppend(t.Context(), "sync", time.Now())
	if err != nil {
		t.Fatalf("OpenAppend() error = %v", err)
	}
	files.api.writeFile = func(windows.Handle, []byte) (int, error) { return 0, errors.New("parent api changed") }
	if _, err := writer.Write([]byte("stored api")); err != nil {
		t.Fatalf("writer.Write() error = %v", err)
	}
	if writeCalls != 1 {
		t.Fatalf("stored write calls = %d, want 1", writeCalls)
	}
	originalClose := files.api.closeHandle
	files.api.closeHandle = func(windows.Handle) error { return errors.New("parent close api changed") }
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}
	files.api.closeHandle = originalClose
}

func TestRuntimeLogFiles_OpenAppendVerifiesDirectoryAndParentIdentity(t *testing.T) {
	layout := newRuntimeLogTestLayout(t)
	api := newProductionPathAPI()
	var directorySpecs []openSpec
	openRelative := api.openRelative
	api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
		if spec.directory {
			directorySpecs = append(directorySpecs, spec)
		}
		return openRelative(parent, name, spec)
	}
	files, err := newRuntimeLogFilesWith(t.Context(), layout, runtimeLogDependencies{api: api})
	if err != nil {
		t.Fatalf("newRuntimeLogFilesWith() error = %v", err)
	}
	t.Cleanup(func() { _ = files.Close() })
	for _, got := range directorySpecs {
		if got != directoryPinSpec() {
			t.Fatalf("directory pin spec = %#v, want %#v", got, directoryPinSpec())
		}
	}
	if len(directorySpecs) < 2 {
		t.Fatalf("directory pin opens = %d, want at least 2", len(directorySpecs))
	}

	leaf := windows.InvalidHandle
	leafClosed := false
	parentSpecSeen := false
	originalOpenRelative := files.api.openRelative
	originalOpenPath := files.api.openPath
	originalIdentity := files.api.identity
	originalClose := files.api.closeHandle
	const mismatchedParent = windows.Handle(9876)
	files.api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
		handle, err := originalOpenRelative(parent, name, spec)
		if err == nil && !spec.directory {
			leaf = handle
		}
		return handle, err
	}
	files.api.openPath = func(path string, spec openSpec) (windows.Handle, error) {
		if spec == parentIdentitySpec() {
			parentSpecSeen = true
			return mismatchedParent, nil
		}
		return originalOpenPath(path, spec)
	}
	files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
		if handle == mismatchedParent {
			return objectIdentity{volumeSerial: 1}, nil
		}
		return originalIdentity(handle)
	}
	files.api.closeHandle = func(handle windows.Handle) error {
		if handle == mismatchedParent {
			return nil
		}
		if handle == leaf {
			leafClosed = true
		}
		return originalClose(handle)
	}
	writer, err := files.OpenAppend(t.Context(), "sync", time.Now())
	if writer != nil || !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("OpenAppend() = %#v, %v, want nil, ErrIdentityChanged", writer, err)
	}
	if !parentSpecSeen {
		t.Fatalf("parent identity spec was not used")
	}
	if !leafClosed {
		t.Fatalf("leaf handle was not closed after parent identity mismatch")
	}
	path, pathErr := layout.RuntimeLogFile("sync", time.Now())
	if pathErr != nil {
		t.Fatalf("RuntimeLogFile() error = %v", pathErr)
	}
	contents, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("os.ReadFile() error = %v", readErr)
	}
	if len(contents) != 0 {
		t.Fatalf("unsafe append wrote %q", contents)
	}
}

func TestRuntimeLogFiles_OpenAppendClosesPartialDuplicatePins(t *testing.T) {
	for failAt := 1; failAt <= 3; failAt++ {
		t.Run("duplicate-"+string(rune('0'+failAt)), func(t *testing.T) {
			files, _ := newRuntimeLogFixture(t)
			originalOpenRelative := files.api.openRelative
			originalClose := files.api.closeHandle
			leaf := windows.InvalidHandle
			closedLeaf := false
			closedDuplicates := make(map[windows.Handle]bool)
			calls := 0
			files.api.openRelative = func(parent windows.Handle, name string, spec openSpec) (windows.Handle, error) {
				handle, err := originalOpenRelative(parent, name, spec)
				if err == nil && !spec.directory {
					leaf = handle
				}
				return handle, err
			}
			files.api.duplicateHandle = func(windows.Handle) (windows.Handle, error) {
				calls++
				if calls == failAt {
					return windows.InvalidHandle, errors.New("injected duplicate failure")
				}
				return windows.Handle(10000 + calls), nil
			}
			files.api.closeHandle = func(handle windows.Handle) error {
				if handle == leaf {
					closedLeaf = true
					return originalClose(handle)
				}
				if handle >= 10000 {
					closedDuplicates[handle] = true
					return nil
				}
				return originalClose(handle)
			}
			writer, err := files.OpenAppend(t.Context(), "sync", time.Now())
			if writer != nil || err == nil {
				t.Fatalf("OpenAppend() = %#v, %v, want nil error", writer, err)
			}
			if !closedLeaf {
				t.Fatal("leaf was not closed after duplicate failure")
			}
			for duplicate := 1; duplicate < failAt; duplicate++ {
				handle := windows.Handle(10000 + duplicate)
				if !closedDuplicates[handle] {
					t.Fatalf("duplicate handle %#x was not closed", handle)
				}
			}
		})
	}
}

func newRuntimeLogFixture(t *testing.T) (*RuntimeLogFiles, *config.Layout) {
	t.Helper()
	layout := newRuntimeLogTestLayout(t)
	files, err := NewRuntimeLogFiles(t.Context(), layout)
	if err != nil {
		t.Fatalf("NewRuntimeLogFiles() error = %v", err)
	}
	t.Cleanup(func() { _ = files.Close() })
	return files, layout
}

func newRuntimeLogTestLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	return layout
}

func assertRuntimeLogAncestorsPinned(t *testing.T, layout *config.Layout) {
	t.Helper()
	for _, path := range []string{layout.RuntimeLogDir(), layout.LogsDir(), layout.AppRoot()} {
		renamed := path + "-renamed"
		if err := os.Rename(path, renamed); err == nil {
			_ = os.Rename(renamed, path)
			t.Fatalf("%q renamed while pinned", path)
		}
	}
}

func assertRuntimeLogAncestorsRenamable(t *testing.T, layout *config.Layout) {
	t.Helper()
	for _, path := range []string{layout.RuntimeLogDir(), layout.LogsDir(), layout.AppRoot()} {
		renamed := path + "-renamed"
		if err := os.Rename(path, renamed); err != nil {
			t.Fatalf("os.Rename(%q) error = %v", path, err)
		}
		if err := os.Rename(renamed, path); err != nil {
			t.Fatalf("restore %q error = %v", path, err)
		}
	}
}
