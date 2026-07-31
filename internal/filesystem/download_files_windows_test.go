package filesystem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestNewDownloadFiles_RejectsInvalidLayoutAndRoot(t *testing.T) {
	if files, err := NewDownloadFiles(nil); files != nil || !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("NewDownloadFiles(nil) = %#v, %v", files, err)
	}
	parent := t.TempDir()
	external := t.TempDir()
	root := filepath.Join(parent, "app")
	if err := os.Symlink(external, root); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	layout, err := config.NewLayout(root, parent)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	if files, err := NewDownloadFiles(layout); files != nil || err == nil {
		t.Fatalf("NewDownloadFiles(reparse root) = %#v, %v, want rejection", files, err)
	}
}

func TestNewDownloadFiles_ValidatesThenClosesAllHandles(t *testing.T) {
	layout := newDownloadTestLayout(t)
	api := newProductionPathAPI()
	openHandles := 0
	openPath := api.openPath
	openRelative := api.openRelative
	closeHandle := api.closeHandle
	api.openPath = func(path string, spec openSpec) (windows.Handle, error) {
		handle, err := openPath(path, spec)
		if err == nil {
			openHandles++
		}
		return handle, err
	}
	api.openRelative = func(
		parent windows.Handle,
		name string,
		spec openSpec,
	) (windows.Handle, error) {
		handle, err := openRelative(parent, name, spec)
		if err == nil {
			openHandles++
		}
		return handle, err
	}
	api.closeHandle = func(handle windows.Handle) error {
		err := closeHandle(handle)
		if err == nil {
			openHandles--
		}
		return err
	}
	files, err := newDownloadFilesWith(layout, downloadFileDependencies{api: api})
	if err != nil {
		t.Fatalf("newDownloadFilesWith() error = %v", err)
	}
	if files == nil || openHandles != 0 {
		t.Fatalf("files/open handles = %#v/%d, want non-nil/0", files, openHandles)
	}
}

func TestNewDownloadFiles_DoesNotCreateOrRetainDirectories(t *testing.T) {
	layout := newDownloadTestLayout(t)
	files, err := NewDownloadFiles(layout)
	if err != nil {
		t.Fatalf("NewDownloadFiles() error = %v", err)
	}
	if files == nil {
		t.Fatal("NewDownloadFiles() = nil, want capability")
	}
	for _, path := range []string{
		layout.RuntimeDir(),
		layout.RuntimeCacheDir(),
		layout.DownloadCacheDir(),
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("os.Stat(%q) error = %v, want not-exist", path, err)
		}
	}
	renamed := layout.AppRoot() + "-renamed"
	if err := os.Rename(layout.AppRoot(), renamed); err != nil {
		t.Fatalf("app root retained by constructor: %v", err)
	}
}

func TestNewDownloadFiles_ValidationCloseFailureReturnsError(t *testing.T) {
	layout := newDownloadTestLayout(t)
	api := newProductionPathAPI()
	closeHandle := api.closeHandle
	injected := errors.New("close failed")
	inject := true
	api.closeHandle = func(handle windows.Handle) error {
		err := closeHandle(handle)
		if inject {
			inject = false
			return errors.Join(injected, err)
		}
		return err
	}
	files, err := newDownloadFilesWith(layout, downloadFileDependencies{api: api})
	if files != nil || !errors.Is(err, injected) {
		t.Fatalf("newDownloadFilesWith() = %#v, %v, want nil/injected", files, err)
	}
}

func TestDownloadFiles_BeginUsesOnlyLayoutPaths(t *testing.T) {
	files, layout := newDownloadFixture(t)
	session, err := files.Begin(t.Context(), "uv.zip")
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	t.Cleanup(func() { _, _ = session.Abort(context.Background()) })
	wantFinal, _ := layout.DownloadFile("uv.zip")
	wantPart, _ := layout.DownloadPartFile("uv.zip")
	if session.Path() != wantFinal || session.PartPath() != wantPart {
		t.Fatalf("session paths = %q/%q, want %q/%q", session.Path(), session.PartPath(), wantFinal, wantPart)
	}
}

func TestDownloadFiles_BeginCreatesAndPinsFourAncestors(t *testing.T) {
	files, layout := newDownloadFixture(t)
	session, err := files.Begin(t.Context(), "uv.zip")
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	for _, path := range []string{
		layout.AppRoot(),
		layout.RuntimeDir(),
		layout.RuntimeCacheDir(),
		layout.DownloadCacheDir(),
	} {
		renamed := path + "-renamed"
		if err := os.Rename(path, renamed); err == nil {
			_ = os.Rename(renamed, path)
			_, _ = session.Abort(t.Context())
			t.Fatalf("ancestor %q renamed while session was open", path)
		}
	}
	if _, err := session.Abort(t.Context()); err != nil {
		t.Fatalf("Abort() error = %v", err)
	}
}

func TestDownloadFiles_BeginOpensFreshAncestorChainEveryTime(t *testing.T) {
	files, layout := newDownloadFixture(t)
	first, err := files.Begin(t.Context(), "first.zip")
	if err != nil {
		t.Fatalf("first Begin() error = %v", err)
	}
	if _, err := first.Abort(t.Context()); err != nil {
		t.Fatalf("first Abort() error = %v", err)
	}
	external := t.TempDir()
	if err := os.Remove(layout.DownloadCacheDir()); err != nil {
		t.Fatalf("os.Remove(downloads) error = %v", err)
	}
	if err := os.Symlink(external, layout.DownloadCacheDir()); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	second, err := files.Begin(t.Context(), "second.zip")
	if second != nil || err == nil {
		if second != nil {
			_, _ = second.Abort(t.Context())
		}
		t.Fatalf("second Begin() = %#v, %v, want reparse rejection", second, err)
	}
}

func TestDownloadFiles_BeginFailureDoesNotRemoveCreatedDirectories(t *testing.T) {
	files, layout := newDownloadFixture(t)
	openRelative := files.api.openRelative
	files.api.openRelative = func(
		parent windows.Handle,
		name string,
		spec openSpec,
	) (windows.Handle, error) {
		if spec.creation == windows.CREATE_NEW {
			return windows.InvalidHandle, errors.New("part create failed")
		}
		return openRelative(parent, name, spec)
	}
	if session, err := files.Begin(t.Context(), "uv.zip"); session != nil || err == nil {
		t.Fatalf("Begin() = %#v, %v, want failure", session, err)
	}
	for _, path := range []string{
		layout.RuntimeDir(),
		layout.RuntimeCacheDir(),
		layout.DownloadCacheDir(),
	} {
		if information, err := os.Stat(path); err != nil || !information.IsDir() {
			t.Fatalf("created directory %q = %v, error = %v", path, information, err)
		}
	}
}

func TestDownloadFiles_BeginRejectsUnsafeFinal(t *testing.T) {
	files, layout := newDownloadFixture(t)
	final, _ := layout.DownloadFile("uv.zip")
	if err := os.MkdirAll(layout.DownloadCacheDir(), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(final, []byte("final"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	if err := os.Link(final, final+".alias"); err != nil {
		t.Skipf("hard-link fixture unavailable: %v", err)
	}
	session, err := files.Begin(t.Context(), "uv.zip")
	if session != nil || !errors.Is(err, ErrUnsafeHardLink) {
		t.Fatalf("Begin() = %#v, %v, want unsafe hard-link rejection", session, err)
	}
}

func TestDownloadFiles_BeginRemovesOnlySafeStalePart(t *testing.T) {
	files, layout := newDownloadFixture(t)
	part, _ := layout.DownloadPartFile("uv.zip")
	if err := os.MkdirAll(layout.DownloadCacheDir(), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(part, []byte("stale"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	session, err := files.Begin(t.Context(), "uv.zip")
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	defer func() { _, _ = session.Abort(t.Context()) }()
	identity, err := session.api.identity(session.part.handle)
	if err != nil || identity.size != 0 {
		t.Fatalf("new part identity = %#v, error = %v, want size 0", identity, err)
	}
}

func TestDownloadFiles_BeginDoesNotStealActivePart(t *testing.T) {
	files, _ := newDownloadFixture(t)
	first, err := files.Begin(t.Context(), "uv.zip")
	if err != nil {
		t.Fatalf("first Begin() error = %v", err)
	}
	defer func() { _, _ = first.Abort(t.Context()) }()
	second, err := files.Begin(t.Context(), "uv.zip")
	if second != nil || err == nil {
		t.Fatalf("second Begin() = %#v, %v, want occupied failure", second, err)
	}
}

func TestDownloadFiles_BeginContextRejectedBeforeIO(t *testing.T) {
	files, _ := newDownloadFixture(t)
	openCalls := 0
	openRelative := files.api.openRelative
	files.api.openRelative = func(
		parent windows.Handle,
		name string,
		spec openSpec,
	) (windows.Handle, error) {
		openCalls++
		return openRelative(parent, name, spec)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if session, err := files.Begin(ctx, "uv.zip"); session != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Begin() = %#v, %v, want nil/context.Canceled", session, err)
	}
	if session, err := files.Begin(nil, "uv.zip"); session != nil || !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Begin(nil) = %#v, %v, want nil/ErrInvalidArgument", session, err)
	}
	if openCalls != 0 {
		t.Fatalf("open calls = %d, want 0", openCalls)
	}
}

func TestDownloadFiles_BeginMatchesAccessMatrixAndParentIdentity(t *testing.T) {
	files, _ := newDownloadFixture(t)
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
	session, err := files.Begin(t.Context(), "uv.zip")
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	defer func() { _, _ = session.Abort(t.Context()) }()
	wantPart := openSpec{
		access: windows.FILE_WRITE_DATA |
			windows.FILE_READ_ATTRIBUTES |
			windows.DELETE |
			windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ,
		creation:  windows.CREATE_NEW,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	}
	if !containsOpenSpec(specs, wantPart) {
		t.Fatalf("open specs = %#v, missing new-part spec", specs)
	}
}

func TestDownloadSession_WriteRevalidatesIdentityAndLinkCount(t *testing.T) {
	files, _ := newDownloadFixture(t)
	session, err := files.Begin(t.Context(), "uv.zip")
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	defer func() { _, _ = session.Abort(t.Context()) }()
	if err := os.Link(session.PartPath(), session.PartPath()+".alias"); err != nil {
		t.Skipf("hard-link fixture unavailable: %v", err)
	}
	if _, err := session.Write([]byte("unsafe")); !errors.Is(err, ErrUnsafeHardLink) {
		t.Fatalf("Write() error = %v, want ErrUnsafeHardLink", err)
	}
}

func TestDownloadSession_WriteAndCloseUseStoredAPI(t *testing.T) {
	files, _ := newDownloadFixture(t)
	writeCalls := 0
	writeFile := files.api.writeFile
	files.api.writeFile = func(handle windows.Handle, payload []byte) (int, error) {
		writeCalls++
		return writeFile(handle, payload)
	}
	session, err := files.Begin(t.Context(), "uv.zip")
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	files.api.writeFile = func(windows.Handle, []byte) (int, error) {
		return 0, errors.New("parent api changed")
	}
	if _, err := session.Write([]byte("stored")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if writeCalls != 1 {
		t.Fatalf("stored write calls = %d, want 1", writeCalls)
	}
	if _, err := session.Abort(t.Context()); err != nil {
		t.Fatalf("Abort() error = %v", err)
	}
}

func TestDownloadSession_AbortContextRejectedBeforeIO(t *testing.T) {
	files, _ := newDownloadFixture(t)
	session, err := files.Begin(t.Context(), "uv.zip")
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	t.Cleanup(func() { _ = session.closeForTest() })
	dispositionCalls := 0
	setDisposition := session.api.setDisposition
	session.api.setDisposition = func(handle windows.Handle) error {
		dispositionCalls++
		return setDisposition(handle)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := session.Abort(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Abort() error = %v, want context.Canceled", err)
	}
	if _, err := session.Abort(nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Abort(nil) error = %v, want ErrInvalidArgument", err)
	}
	if dispositionCalls != 0 {
		t.Fatalf("disposition calls = %d, want 0", dispositionCalls)
	}
}

func TestDownloadFiles_ConcurrentDifferentNames(t *testing.T) {
	files, _ := newDownloadFixture(t)
	const count = 32
	start := make(chan struct{})
	errs := make(chan error, count)
	var wait sync.WaitGroup
	wait.Add(count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("part-%02d.zip", i)
		go func() {
			defer wait.Done()
			<-start
			session, err := files.Begin(t.Context(), name)
			if err == nil {
				_, err = session.Write([]byte(name))
			}
			if err == nil {
				_, err = session.Abort(t.Context())
			}
			errs <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent session error = %v", err)
		}
	}
}

func TestDownloadSession_PublishNoReplaceFlushesAndKeepsFileID(t *testing.T) {
	session := newDownloadSessionForTest(t)
	if _, err := session.Write([]byte("payload")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	originalID := session.part.identity.fileID
	result, err := session.PublishNoReplace(t.Context())
	if err != nil || !result.Published {
		t.Fatalf("PublishNoReplace() = %#v, %v", result, err)
	}
	got, err := os.ReadFile(session.Path())
	if err != nil || string(got) != "payload" {
		t.Fatalf("final payload = %q, error = %v", got, err)
	}
	api := newProductionPathAPI()
	handle, err := api.openPath(nativeWindowsPath(session.Path()), openSpec{
		access:    windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: false,
	})
	if err != nil {
		t.Fatalf("open final error = %v", err)
	}
	identity, identityErr := api.identity(handle)
	closeErr := api.closeHandle(handle)
	if identityErr != nil || closeErr != nil {
		t.Fatalf("final identity/close errors = %v/%v", identityErr, closeErr)
	}
	if identity.fileID != originalID {
		t.Fatalf("final file ID = %x, want %x", identity.fileID, originalID)
	}
}

func TestDownloadSession_PublishNeverReplacesCompetitor(t *testing.T) {
	session := newDownloadSessionForTest(t)
	if _, err := session.Write([]byte("session")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := os.WriteFile(session.Path(), []byte("competitor"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(competitor) error = %v", err)
	}
	result, err := session.PublishNoReplace(t.Context())
	if result.Published || !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("PublishNoReplace() = %#v, %v, want occupied", result, err)
	}
	got, _ := os.ReadFile(session.Path())
	if string(got) != "competitor" {
		t.Fatalf("competitor = %q, want unchanged", got)
	}
	if _, err := session.Abort(t.Context()); err != nil {
		t.Fatalf("Abort() error = %v", err)
	}
}

func TestDownloadSession_PublishFailureRemainsAbortable(t *testing.T) {
	session := newDownloadSessionForTest(t)
	injected := errors.New("rename failed")
	session.api.rename = func(
		windows.Handle,
		windows.Handle,
		string,
		bool,
	) error {
		return injected
	}
	result, err := session.PublishNoReplace(t.Context())
	if result.Published || !errors.Is(err, injected) {
		t.Fatalf("PublishNoReplace() = %#v, %v", result, err)
	}
	abort, err := session.Abort(t.Context())
	if err != nil || !abort.Removed {
		t.Fatalf("Abort() = %#v, %v, want removed", abort, err)
	}
}

func TestDownloadSession_PublishReportsPublishedBeforeCloseError(t *testing.T) {
	session := newDownloadSessionForTest(t)
	injected := errors.New("close failed")
	closeHandle := session.api.closeHandle
	rename := session.api.rename
	renamed := false
	session.api.rename = func(
		source windows.Handle,
		parent windows.Handle,
		name string,
		replace bool,
	) error {
		err := rename(source, parent, name, replace)
		renamed = err == nil
		return err
	}
	session.api.closeHandle = func(handle windows.Handle) error {
		err := closeHandle(handle)
		if renamed {
			renamed = false
			return errors.Join(injected, err)
		}
		return err
	}
	result, err := session.PublishNoReplace(t.Context())
	if !result.Published || !errors.Is(err, injected) {
		t.Fatalf("PublishNoReplace() = %#v, %v, want published/injected", result, err)
	}
	if _, abortErr := session.Abort(t.Context()); !errors.Is(abortErr, ErrClosed) {
		t.Fatalf("Abort() error = %v, want ErrClosed", abortErr)
	}
}

func TestDownloadSession_AbortDeletesBySameHandle(t *testing.T) {
	session := newDownloadSessionForTest(t)
	originalID := session.part.identity.fileID
	seenID := [16]byte{}
	identity := session.api.identity
	setDisposition := session.api.setDisposition
	session.api.setDisposition = func(handle windows.Handle) error {
		got, err := identity(handle)
		if err != nil {
			return err
		}
		seenID = got.fileID
		return setDisposition(handle)
	}
	result, err := session.Abort(t.Context())
	if err != nil || !result.Removed {
		t.Fatalf("Abort() = %#v, %v", result, err)
	}
	if seenID != originalID {
		t.Fatalf("disposed file ID = %x, want %x", seenID, originalID)
	}
}

func TestDownloadSession_AbortIsIdempotentAfterCloseError(t *testing.T) {
	session := newDownloadSessionForTest(t)
	injected := errors.New("close failed")
	closeHandle := session.api.closeHandle
	dispositionCalls := 0
	setDisposition := session.api.setDisposition
	session.api.setDisposition = func(handle windows.Handle) error {
		dispositionCalls++
		return setDisposition(handle)
	}
	session.api.closeHandle = func(handle windows.Handle) error {
		err := closeHandle(handle)
		return errors.Join(injected, err)
	}
	first, firstErr := session.Abort(t.Context())
	second, secondErr := session.Abort(t.Context())
	if !first.Removed || second != first ||
		!errors.Is(firstErr, injected) || !errors.Is(secondErr, injected) {
		t.Fatalf("Abort results = %#v/%#v, errors = %v/%v", first, second, firstErr, secondErr)
	}
	if dispositionCalls != 1 {
		t.Fatalf("disposition calls = %d, want 1", dispositionCalls)
	}
}

func TestDownloadSession_AbortRejectsPublishedSession(t *testing.T) {
	session := newDownloadSessionForTest(t)
	if result, err := session.PublishNoReplace(t.Context()); err != nil || !result.Published {
		t.Fatalf("PublishNoReplace() = %#v, %v", result, err)
	}
	if result, err := session.Abort(t.Context()); result.Removed || !errors.Is(err, ErrClosed) {
		t.Fatalf("Abort() = %#v, %v, want state rejection", result, err)
	}
}

func TestDownloadSession_StateTransitionsAreLinearized(t *testing.T) {
	session := newDownloadSessionForTest(t)
	start := make(chan struct{})
	type outcome struct {
		published bool
		removed   bool
		err       error
	}
	outcomes := make(chan outcome, 2)
	go func() {
		<-start
		result, err := session.PublishNoReplace(t.Context())
		outcomes <- outcome{published: result.Published, err: err}
	}()
	go func() {
		<-start
		result, err := session.Abort(t.Context())
		outcomes <- outcome{removed: result.Removed, err: err}
	}()
	close(start)
	first := <-outcomes
	second := <-outcomes
	successes := 0
	for _, result := range []outcome{first, second} {
		if result.published || result.removed {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("transition outcomes = %#v/%#v, want exactly one success", first, second)
	}
}

func TestDownloadSession_PublishAndAbortContextsRejectedBeforeIO(t *testing.T) {
	session := newDownloadSessionForTest(t)
	t.Cleanup(func() { _ = session.closeForTest() })
	flushCalls := 0
	dispositionCalls := 0
	session.api.flushFile = func(windows.Handle) error {
		flushCalls++
		return nil
	}
	session.api.setDisposition = func(windows.Handle) error {
		dispositionCalls++
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := session.PublishNoReplace(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("PublishNoReplace() error = %v", err)
	}
	if _, err := session.Abort(nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Abort(nil) error = %v", err)
	}
	if flushCalls != 0 || dispositionCalls != 0 {
		t.Fatalf("I/O calls = flush %d/disposition %d, want 0/0", flushCalls, dispositionCalls)
	}
}

func TestDownloadSession_PublishCancellationAfterRenameKeepsPublished(t *testing.T) {
	session := newDownloadSessionForTest(t)
	ctx, cancel := context.WithCancel(t.Context())
	rename := session.api.rename
	session.api.rename = func(
		source windows.Handle,
		parent windows.Handle,
		name string,
		replace bool,
	) error {
		err := rename(source, parent, name, replace)
		if err == nil {
			cancel()
		}
		return err
	}
	result, err := session.PublishNoReplace(ctx)
	if err != nil || !result.Published {
		t.Fatalf("PublishNoReplace() = %#v, %v, want published/nil", result, err)
	}
}

func TestDownloadSession_AbortCancellationAfterDispositionKeepsRemoved(t *testing.T) {
	session := newDownloadSessionForTest(t)
	ctx, cancel := context.WithCancel(t.Context())
	setDisposition := session.api.setDisposition
	session.api.setDisposition = func(handle windows.Handle) error {
		err := setDisposition(handle)
		if err == nil {
			cancel()
		}
		return err
	}
	result, err := session.Abort(ctx)
	if err != nil || !result.Removed {
		t.Fatalf("Abort() = %#v, %v, want removed/nil", result, err)
	}
}

func TestWindows_DownloadPublishesValidatedHandleWithoutReplacement(t *testing.T) {
	session := newDownloadSessionForTest(t)
	if _, err := session.Write([]byte("validated")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if result, err := session.PublishNoReplace(t.Context()); err != nil || !result.Published {
		t.Fatalf("PublishNoReplace() = %#v, %v", result, err)
	}
	got, err := os.ReadFile(session.Path())
	if err != nil || string(got) != "validated" {
		t.Fatalf("published file = %q, error = %v", got, err)
	}
	if _, err := os.Stat(session.PartPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("part path still exists: %v", err)
	}
}

func newDownloadSessionForTest(t *testing.T) *DownloadSession {
	t.Helper()
	files, _ := newDownloadFixture(t)
	session, err := files.Begin(t.Context(), "uv.zip")
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	t.Cleanup(func() { _ = session.closeForTest() })
	return session
}

func newDownloadFixture(t *testing.T) (*DownloadFiles, *config.Layout) {
	t.Helper()
	layout := newDownloadTestLayout(t)
	files, err := NewDownloadFiles(layout)
	if err != nil {
		t.Fatalf("NewDownloadFiles() error = %v", err)
	}
	return files, layout
}

func newDownloadTestLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	return layout
}

func (s *DownloadSession) closeForTest() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	closeErrors := make([]error, 0, len(s.pins)+1)
	closeOne := func(handle *windows.Handle) {
		if *handle == 0 || *handle == windows.InvalidHandle {
			return
		}
		if err := s.api.closeHandle(*handle); err != nil {
			closeErrors = append(closeErrors, err)
			return
		}
		*handle = windows.InvalidHandle
	}
	closeOne(&s.part.handle)
	for i := len(s.pins) - 1; i >= 0; i-- {
		closeOne(&s.pins[i].handle)
	}
	return errors.Join(closeErrors...)
}
