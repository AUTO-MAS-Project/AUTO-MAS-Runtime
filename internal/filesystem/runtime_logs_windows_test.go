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

func TestRuntimeLogFiles_OpenAppendClassifiesLeafReparseAfterTypeConstrainedOpenFailure(
	t *testing.T,
) {
	files, _ := newRuntimeLogFixture(t)
	files.api.openRelative = func(
		windows.Handle,
		string,
		openSpec,
	) (windows.Handle, error) {
		return windows.InvalidHandle, windows.ERROR_ACCESS_DENIED
	}

	const probeHandle = windows.Handle(0x4242)
	var got ntCreateSpec
	probeCalls := 0
	files.api.ntCreateRelative = func(
		parent windows.Handle,
		name string,
		spec ntCreateSpec,
	) (windows.Handle, error) {
		probeCalls++
		if parent != files.pins[2].handle {
			t.Fatalf("classification parent = %#x, want pinned runtime-log parent %#x", parent, files.pins[2].handle)
		}
		if name == "" || filepath.Base(name) != name {
			t.Fatalf("classification leaf = %q, want one relative name", name)
		}
		got = spec
		return probeHandle, nil
	}
	identity := files.api.identity
	files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
		if handle == probeHandle {
			return objectIdentity{
				attributes: windows.FILE_ATTRIBUTE_DIRECTORY |
					windows.FILE_ATTRIBUTE_REPARSE_POINT,
			}, nil
		}
		return identity(handle)
	}
	closeHandle := files.api.closeHandle
	probeClosed := false
	files.api.closeHandle = func(handle windows.Handle) error {
		if handle == probeHandle {
			probeClosed = true
			return nil
		}
		return closeHandle(handle)
	}

	writer, err := files.OpenAppend(t.Context(), "sync", time.Now())
	if writer != nil {
		_ = writer.Close()
		t.Fatal("OpenAppend() returned a writer through a reparse point")
	}
	assertFilesystemCode(t, err, protocol.CodeUnsafeReparsePoint)
	if probeCalls != 1 {
		t.Fatalf("classification probe calls = %d, want 1", probeCalls)
	}
	want := ntCreateSpec{
		desiredAccess: windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		shareAccess: windows.FILE_SHARE_READ |
			windows.FILE_SHARE_WRITE |
			windows.FILE_SHARE_DELETE,
		createDisposition: ntFileOpen,
		createOptions: ntFileOpenReparsePoint |
			ntFileSynchronousNonalert,
	}
	if got != want {
		t.Fatalf("classification ntCreateSpec = %#v, want %#v", got, want)
	}
	if got.createOptions&(ntFileDirectoryFile|ntFileNonDirectoryFile) != 0 {
		t.Fatalf("classification create options = %#x, want no type constraint", got.createOptions)
	}
	if !probeClosed {
		t.Fatal("classification probe handle was not closed")
	}
}

func TestRuntimeLogFiles_OpenAppendClassificationFailureMatrix(t *testing.T) {
	originalErr := windows.ERROR_ACCESS_DENIED
	probeOpenErr := errors.New("probe open failed")
	identityErr := errors.New("probe identity failed")
	closeErr := errors.New("probe close failed")
	tests := []struct {
		name              string
		probeOpenErr      error
		identity          objectIdentity
		identityErr       error
		closeErr          error
		wantProbeOpenErr  bool
		wantIdentityErr   bool
		wantCloseErr      bool
		wantUnsafeReparse bool
		wantClosed        bool
	}{
		{
			name:             "probe open failure",
			probeOpenErr:     probeOpenErr,
			wantProbeOpenErr: true,
		},
		{
			name:            "probe identity failure",
			identityErr:     identityErr,
			wantIdentityErr: true,
			wantClosed:      true,
		},
		{
			name:       "ordinary object preserves original",
			wantClosed: true,
		},
		{
			name:         "ordinary object close failure",
			closeErr:     closeErr,
			wantCloseErr: true,
			wantClosed:   true,
		},
		{
			name: "reparse object close failure",
			identity: objectIdentity{
				attributes: windows.FILE_ATTRIBUTE_REPARSE_POINT,
			},
			closeErr:          closeErr,
			wantCloseErr:      true,
			wantUnsafeReparse: true,
			wantClosed:        true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files, _ := newRuntimeLogFixture(t)
			files.api.openRelative = func(
				windows.Handle,
				string,
				openSpec,
			) (windows.Handle, error) {
				return windows.InvalidHandle, originalErr
			}

			const probeHandle = windows.Handle(0x4343)
			files.api.ntCreateRelative = func(
				windows.Handle,
				string,
				ntCreateSpec,
			) (windows.Handle, error) {
				if test.probeOpenErr != nil {
					return windows.InvalidHandle, test.probeOpenErr
				}
				return probeHandle, nil
			}
			identity := files.api.identity
			files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
				if handle == probeHandle {
					return test.identity, test.identityErr
				}
				return identity(handle)
			}
			closeHandle := files.api.closeHandle
			closed := false
			files.api.closeHandle = func(handle windows.Handle) error {
				if handle == probeHandle {
					closed = true
					return test.closeErr
				}
				return closeHandle(handle)
			}

			writer, err := files.OpenAppend(t.Context(), "sync", time.Now())
			if writer != nil {
				_ = writer.Close()
				t.Fatal("OpenAppend() returned a writer after append-open failure")
			}
			if !errors.Is(err, originalErr) {
				t.Fatalf("OpenAppend() error = %v, want original append-open error", err)
			}
			if got := errors.Is(err, probeOpenErr); got != test.wantProbeOpenErr {
				t.Fatalf("errors.Is(probeOpenErr) = %t, want %t", got, test.wantProbeOpenErr)
			}
			if got := errors.Is(err, identityErr); got != test.wantIdentityErr {
				t.Fatalf("errors.Is(identityErr) = %t, want %t", got, test.wantIdentityErr)
			}
			if got := errors.Is(err, closeErr); got != test.wantCloseErr {
				t.Fatalf("errors.Is(closeErr) = %t, want %t", got, test.wantCloseErr)
			}
			var stable *Error
			gotUnsafeReparse := errors.As(err, &stable) &&
				stable.Code() == protocol.CodeUnsafeReparsePoint
			if gotUnsafeReparse != test.wantUnsafeReparse {
				t.Fatalf(
					"unsafe reparse classification = %t, want %t; error = %v",
					gotUnsafeReparse,
					test.wantUnsafeReparse,
					err,
				)
			}
			if closed != test.wantClosed {
				t.Fatalf("classification probe closed = %t, want %t", closed, test.wantClosed)
			}
		})
	}
}

func TestRuntimeLogFiles_OpenAppendDoesNotClassifyPostOpenValidationFailure(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	const leafHandle = windows.Handle(0x4444)
	files.api.openRelative = func(
		windows.Handle,
		string,
		openSpec,
	) (windows.Handle, error) {
		return leafHandle, nil
	}
	identityErr := errors.New("leaf identity failed")
	identity := files.api.identity
	files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
		if handle == leafHandle {
			return objectIdentity{}, identityErr
		}
		return identity(handle)
	}
	closeHandle := files.api.closeHandle
	files.api.closeHandle = func(handle windows.Handle) error {
		if handle == leafHandle {
			return nil
		}
		return closeHandle(handle)
	}
	probeCalls := 0
	files.api.ntCreateRelative = func(
		windows.Handle,
		string,
		ntCreateSpec,
	) (windows.Handle, error) {
		probeCalls++
		return windows.InvalidHandle, errors.New("unexpected classification probe")
	}

	writer, err := files.OpenAppend(t.Context(), "sync", time.Now())
	if writer != nil {
		_ = writer.Close()
		t.Fatal("OpenAppend() returned a writer after identity failure")
	}
	if !errors.Is(err, identityErr) {
		t.Fatalf("OpenAppend() error = %v, want identity failure", err)
	}
	if probeCalls != 0 {
		t.Fatalf("classification probe calls = %d, want 0 after post-open failure", probeCalls)
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

func TestRuntimeLogFiles_ListReturnsOnlyDirectSafeFiles(t *testing.T) {
	files, layout := newRuntimeLogFixture(t)
	first := createRuntimeLogToken(t, files, "sync")
	second := createRuntimeLogToken(t, files, "doctor")
	subdirectory := filepath.Join(layout.RuntimeLogDir(), "nested")
	if err := os.Mkdir(subdirectory, 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(subdirectory, "hidden.log"), []byte("x"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	listed, err := files.List(t.Context())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	names := map[string]bool{}
	for _, file := range listed {
		names[file.Name()] = true
	}
	for _, want := range []string{first.Name(), second.Name()} {
		if !names[want] {
			t.Errorf("List() names = %v, missing %q", names, want)
		}
	}
	if names["hidden.log"] {
		t.Fatal("List() returned a nested file")
	}
}

func TestRuntimeLogFiles_ListSkipsDirectoriesReparseAndHardLinks(t *testing.T) {
	files, layout := newRuntimeLogFixture(t)
	token := createRuntimeLogToken(t, files, "sync")
	alias := token.Path() + ".alias"
	if err := os.Link(token.Path(), alias); err != nil {
		t.Skipf("hard-link fixture unavailable: %v", err)
	}
	if err := os.Mkdir(filepath.Join(layout.RuntimeLogDir(), "directory"), 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	external := filepath.Join(t.TempDir(), "external.log")
	if err := os.WriteFile(external, []byte("external"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	link := filepath.Join(layout.RuntimeLogDir(), "link.log")
	if err := os.Symlink(external, link); err != nil {
		t.Logf("file symlink unavailable: %v", err)
	}
	listed, err := files.List(t.Context())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, file := range listed {
		if file.Path() == token.Path() || file.Path() == alias || file.Path() == link {
			t.Fatalf("List() returned unsafe file %q", file.Path())
		}
	}
}

func TestRuntimeLogFiles_RemoveRejectsZeroAndForeignTokens(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	if result, err := files.Remove(t.Context(), RuntimeLogFile{}); result.MutationApplied ||
		!errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Remove(zero) = %#v, %v, want false/ErrInvalidToken", result, err)
	}
	foreignFiles, _ := newRuntimeLogFixture(t)
	foreign := createRuntimeLogToken(t, foreignFiles, "sync")
	if result, err := files.Remove(t.Context(), foreign); result.MutationApplied ||
		!errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Remove(foreign) = %#v, %v, want false/ErrInvalidToken", result, err)
	}
}

func TestRuntimeLogFiles_RemoveRejectsReplacedIdentity(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	token := createRuntimeLogToken(t, files, "sync")
	if err := os.Remove(token.Path()); err != nil {
		t.Fatalf("os.Remove() error = %v", err)
	}
	if err := os.WriteFile(token.Path(), []byte("replacement"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	result, err := files.Remove(t.Context(), token)
	if result.MutationApplied || !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("Remove() = %#v, %v, want false/ErrIdentityChanged", result, err)
	}
	got, err := os.ReadFile(token.Path())
	if err != nil || string(got) != "replacement" {
		t.Fatalf("replacement = %q, error = %v", got, err)
	}
}

func TestRuntimeLogFiles_RemoveMissingIsIdempotent(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	token := createRuntimeLogToken(t, files, "sync")
	if err := os.Remove(token.Path()); err != nil {
		t.Fatalf("os.Remove() error = %v", err)
	}
	result, err := files.Remove(t.Context(), token)
	if err != nil || result.MutationApplied {
		t.Fatalf("Remove() = %#v, %v, want zero/nil", result, err)
	}
}

func TestRuntimeLogFiles_RemoveReportsAppliedBeforeCloseError(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	token := createRuntimeLogToken(t, files, "sync")
	injected := errors.New("close failed")
	closeHandle := files.api.closeHandle
	inject := false
	setDisposition := files.api.setDisposition
	files.api.setDisposition = func(handle windows.Handle) error {
		err := setDisposition(handle)
		inject = err == nil
		return err
	}
	files.api.closeHandle = func(handle windows.Handle) error {
		err := closeHandle(handle)
		if inject {
			inject = false
			return errors.Join(injected, err)
		}
		return err
	}
	result, err := files.Remove(t.Context(), token)
	if !result.MutationApplied || !errors.Is(err, injected) {
		t.Fatalf("Remove() = %#v, %v, want applied/injected", result, err)
	}
}

func TestRuntimeLogFiles_RemoveNeverDeletesHardLinkTarget(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	token := createRuntimeLogToken(t, files, "sync")
	alias := token.Path() + ".alias"
	if err := os.Link(token.Path(), alias); err != nil {
		t.Skipf("hard-link fixture unavailable: %v", err)
	}
	result, err := files.Remove(t.Context(), token)
	if result.MutationApplied || !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("Remove() = %#v, %v, want false/identity-changed", result, err)
	}
	if _, err := os.Stat(alias); err != nil {
		t.Fatalf("hard-link target changed: %v", err)
	}
}

func TestRuntimeLogFiles_ListAndRemoveRejectAfterClose(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	token := createRuntimeLogToken(t, files, "sync")
	if err := files.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := files.List(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("List() error = %v, want ErrClosed", err)
	}
	if _, err := files.Remove(t.Context(), token); !errors.Is(err, ErrClosed) {
		t.Fatalf("Remove() error = %v, want ErrClosed", err)
	}
}

func TestRuntimeLogFiles_ListAndRemoveContextsRejectedBeforeIO(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	token := createRuntimeLogToken(t, files, "sync")
	listCalls := 0
	removeCalls := 0
	listDirectory := files.api.listDirectory
	setDisposition := files.api.setDisposition
	files.api.listDirectory = func(handle windows.Handle) ([]directoryEntry, error) {
		listCalls++
		return listDirectory(handle)
	}
	files.api.setDisposition = func(handle windows.Handle) error {
		removeCalls++
		return setDisposition(handle)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := files.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("List() error = %v, want context.Canceled", err)
	}
	if _, err := files.Remove(nil, token); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Remove(nil) error = %v, want ErrInvalidArgument", err)
	}
	if listCalls != 0 || removeCalls != 0 {
		t.Fatalf("I/O calls = list %d/remove %d, want 0/0", listCalls, removeCalls)
	}
}

func TestRuntimeLogFiles_ListAndRemoveMatchAccessMatrixAndParentIdentity(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	token := createRuntimeLogToken(t, files, "sync")
	var specs []openSpec
	openRelative := files.api.openRelative
	files.api.openRelative = func(
		parent windows.Handle,
		name string,
		spec openSpec,
	) (windows.Handle, error) {
		specs = append(specs, spec)
		return openRelative(parent, name, spec)
	}
	if _, err := files.List(t.Context()); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if _, err := files.Remove(t.Context(), token); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	wantList := openSpec{
		access:    windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}
	wantRemove := openSpec{
		access:    windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}
	if !containsOpenSpec(specs, wantList) || !containsOpenSpec(specs, wantRemove) {
		t.Fatalf("open specs = %#v, want list and remove specs", specs)
	}
}

func TestRuntimeLogFiles_RemoveCancellationAfterDispositionKeepsApplied(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	token := createRuntimeLogToken(t, files, "sync")
	ctx, cancel := context.WithCancel(t.Context())
	setDisposition := files.api.setDisposition
	files.api.setDisposition = func(handle windows.Handle) error {
		err := setDisposition(handle)
		cancel()
		return err
	}
	result, err := files.Remove(ctx, token)
	if err != nil || !result.MutationApplied {
		t.Fatalf("Remove() = %#v, %v, want applied/nil", result, err)
	}
}

func TestRuntimeLogFiles_RemoveUsesStoredAPIForCanonicalization(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	token := createRuntimeLogToken(t, files, "sync")

	attributeCalls := 0
	attributes := files.api.attributes
	files.api.attributes = func(path string) (uint32, error) {
		attributeCalls++
		return attributes(path)
	}

	result, err := files.Remove(t.Context(), token)
	if err != nil || !result.MutationApplied {
		t.Fatalf("Remove() = %#v, %v, want applied/nil", result, err)
	}
	if attributeCalls == 0 {
		t.Fatal("Remove() bypassed the stored path API during canonicalization")
	}
}

func TestRuntimeLogFiles_RemoveRejectsParentIdentityMismatch(t *testing.T) {
	files, _ := newRuntimeLogFixture(t)
	token := createRuntimeLogToken(t, files, "sync")

	var parentHandle windows.Handle
	openPath := files.api.openPath
	files.api.openPath = func(path string, spec openSpec) (windows.Handle, error) {
		handle, err := openPath(path, spec)
		if err == nil && spec == parentIdentitySpec() {
			parentHandle = handle
		}
		return handle, err
	}
	identity := files.api.identity
	files.api.identity = func(handle windows.Handle) (objectIdentity, error) {
		got, err := identity(handle)
		if err == nil && handle == parentHandle && parentHandle != windows.InvalidHandle {
			got.fileID[0] ^= 0xff
		}
		return got, err
	}

	result, err := files.Remove(t.Context(), token)
	if result.MutationApplied || !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("Remove() = %#v, %v, want false/ErrIdentityChanged", result, err)
	}
	if _, err := os.Stat(token.Path()); err != nil {
		t.Fatalf("token target changed after parent mismatch: %v", err)
	}
}

func TestRuntimeLogFiles_CloseWaitsForStartedOperation(t *testing.T) {
	for _, operation := range []string{"open", "list", "remove"} {
		t.Run(operation, func(t *testing.T) {
			assertRuntimeLogCloseWaitsForOperation(t, operation)
		})
	}
}

func assertRuntimeLogCloseWaitsForOperation(t *testing.T, operation string) {
	t.Helper()
	files, _ := newRuntimeLogFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var run func() error

	switch operation {
	case "open":
		openRelative := files.api.openRelative
		files.api.openRelative = func(
			parent windows.Handle,
			name string,
			spec openSpec,
		) (windows.Handle, error) {
			close(entered)
			<-release
			return openRelative(parent, name, spec)
		}
		run = func() error {
			writer, err := files.OpenAppend(t.Context(), "sync", time.Now())
			if err != nil {
				return err
			}
			return writer.Close()
		}
	case "list":
		listDirectory := files.api.listDirectory
		files.api.listDirectory = func(handle windows.Handle) ([]directoryEntry, error) {
			close(entered)
			<-release
			return listDirectory(handle)
		}
		run = func() error {
			_, err := files.List(t.Context())
			return err
		}
	case "remove":
		token := createRuntimeLogToken(t, files, "sync")
		setDisposition := files.api.setDisposition
		files.api.setDisposition = func(handle windows.Handle) error {
			close(entered)
			<-release
			return setDisposition(handle)
		}
		run = func() error {
			_, err := files.Remove(t.Context(), token)
			return err
		}
	default:
		t.Fatalf("unknown operation %q", operation)
	}

	operationDone := make(chan error, 1)
	go func() { operationDone <- run() }()
	<-entered
	if files.mu.TryLock() {
		files.mu.Unlock()
		t.Fatal("operation did not hold the RuntimeLogFiles mutex")
	}
	closeStarted := make(chan struct{})
	closeDone := make(chan error, 1)
	go func() {
		close(closeStarted)
		closeDone <- files.Close()
	}()
	<-closeStarted
	select {
	case err := <-closeDone:
		t.Fatalf("Close() returned before %s: %v", operation, err)
	default:
	}
	close(release)
	if err := <-operationDone; err != nil {
		t.Fatalf("%s error = %v", operation, err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if writer, err := files.OpenAppend(
		t.Context(),
		"sync",
		time.Now(),
	); writer != nil || !errors.Is(err, ErrClosed) {
		t.Fatalf("OpenAppend() after Close = %#v, %v", writer, err)
	}
	if listed, err := files.List(t.Context()); listed != nil || !errors.Is(err, ErrClosed) {
		t.Fatalf("List() after Close = %#v, %v", listed, err)
	}
	if result, err := files.Remove(
		t.Context(),
		RuntimeLogFile{},
	); result.MutationApplied || !errors.Is(err, ErrClosed) {
		t.Fatalf("Remove() after Close = %#v, %v", result, err)
	}
}

func TestWindows_RuntimeLogPinsAndTokensSurviveReplacementRaces(t *testing.T) {
	files, layout := newRuntimeLogFixture(t)
	token := createRuntimeLogToken(t, files, "sync")
	if err := os.Remove(token.Path()); err != nil {
		t.Fatalf("os.Remove() error = %v", err)
	}
	if err := os.WriteFile(token.Path(), []byte("competitor"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	if _, err := files.Remove(t.Context(), token); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("Remove() error = %v, want ErrIdentityChanged", err)
	}
	if err := os.Rename(layout.RuntimeLogDir(), layout.RuntimeLogDir()+"-other"); err == nil {
		t.Fatal("runtime directory replacement succeeded while capability was alive")
	}
}

func TestWindows_RuntimeLogSharedAppendBlocksDeleteUntilLastWriterCloses(t *testing.T) {
	layout := newRuntimeLogTestLayout(t)
	filesOne, err := NewRuntimeLogFiles(t.Context(), layout)
	if err != nil {
		t.Fatalf("first NewRuntimeLogFiles() error = %v", err)
	}
	t.Cleanup(func() {
		if err := filesOne.Close(); err != nil {
			t.Errorf("first RuntimeLogFiles.Close() error = %v", err)
		}
	})
	filesTwo, err := NewRuntimeLogFiles(t.Context(), layout)
	if err != nil {
		t.Fatalf("second NewRuntimeLogFiles() error = %v", err)
	}
	t.Cleanup(func() {
		if err := filesTwo.Close(); err != nil {
			t.Errorf("second RuntimeLogFiles.Close() error = %v", err)
		}
	})

	date := time.Date(2026, 7, 29, 12, 0, 0, 0, time.Local)
	writerOne, err := filesOne.OpenAppend(t.Context(), "sync", date)
	if err != nil {
		t.Fatalf("first OpenAppend() error = %v", err)
	}
	t.Cleanup(func() {
		if err := writerOne.Close(); err != nil {
			t.Errorf("first writer.Close() error = %v", err)
		}
	})
	if _, err := writerOne.Write([]byte("first\n")); err != nil {
		t.Fatalf("first writer.Write() error = %v", err)
	}

	barrierCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	openSecond := make(chan struct{})
	type writerResult struct {
		writer *RuntimeLogWriter
		err    error
	}
	secondOpened := make(chan writerResult, 1)
	go func() {
		select {
		case <-openSecond:
		case <-barrierCtx.Done():
			secondOpened <- writerResult{err: barrierCtx.Err()}
			return
		}
		writer, openErr := filesTwo.OpenAppend(barrierCtx, "sync", date)
		if openErr == nil {
			if _, writeErr := writer.Write([]byte("second\n")); writeErr != nil {
				openErr = errors.Join(writeErr, writer.Close())
				writer = nil
			}
		}
		secondOpened <- writerResult{writer: writer, err: openErr}
	}()
	close(openSecond)

	var opened writerResult
	select {
	case opened = <-secondOpened:
	case <-barrierCtx.Done():
		t.Fatalf("second writer barrier error = %v", barrierCtx.Err())
	}
	if opened.err != nil || opened.writer == nil {
		t.Fatalf("second OpenAppend/write = %#v, %v, want live writer", opened.writer, opened.err)
	}
	writerTwo := opened.writer
	t.Cleanup(func() {
		if err := writerTwo.Close(); err != nil {
			t.Errorf("second writer.Close() error = %v", err)
		}
	})

	listed, err := filesOne.List(t.Context())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var token RuntimeLogFile
	for _, file := range listed {
		if file.Path() == writerOne.Path() {
			token = file
			break
		}
	}
	if token.Path() == "" {
		t.Fatalf("List() did not return %q", writerOne.Path())
	}

	result, err := filesOne.Remove(t.Context(), token)
	if result.MutationApplied || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("Remove() with two writers = %#v, %v, want false/sharing violation", result, err)
	}
	if err := writerOne.Close(); err != nil {
		t.Fatalf("first writer.Close() error = %v", err)
	}
	result, err = filesOne.Remove(t.Context(), token)
	if result.MutationApplied || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("Remove() with second writer = %#v, %v, want false/sharing violation", result, err)
	}
	if err := writerTwo.Close(); err != nil {
		t.Fatalf("second writer.Close() error = %v", err)
	}

	got, err := os.ReadFile(writerOne.Path())
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if string(got) != "first\nsecond\n" {
		t.Fatalf("log contents = %q, want both appends", got)
	}
	result, err = filesOne.Remove(t.Context(), token)
	if err != nil || !result.MutationApplied {
		t.Fatalf("Remove() after final close = %#v, %v, want true/nil", result, err)
	}
	if _, err := os.Stat(writerOne.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("os.Stat() after Remove error = %v, want os.ErrNotExist", err)
	}
}

func createRuntimeLogToken(
	t *testing.T,
	files *RuntimeLogFiles,
	command string,
) RuntimeLogFile {
	t.Helper()
	writer, err := files.OpenAppend(t.Context(), command, time.Now())
	if err != nil {
		t.Fatalf("OpenAppend() error = %v", err)
	}
	if _, err := writer.Write([]byte(command)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}
	listed, err := files.List(t.Context())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, file := range listed {
		if file.Path() == writer.Path() {
			return file
		}
	}
	t.Fatalf("List() did not return %q", writer.Path())
	return RuntimeLogFile{}
}

func containsOpenSpec(values []openSpec, want openSpec) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
