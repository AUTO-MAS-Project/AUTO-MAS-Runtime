package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestRecovery_ClassifyPathPinsRepositoryDuringReader(t *testing.T) {
	layout := mustGitLayout(t)
	writeRecoveryRepository(t, layout.RepoDir(), "v5.4.0", recoverySourceURL(t), "active")
	snapshot, err := inspectTestRepository(t, layout.RepoDir())
	if err != nil {
		t.Fatalf("Inspect(repository) error = %v", err)
	}
	reader := &blockingServiceReader{
		snapshot: snapshot,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	recovery, err := newRecoveryWithDependencies(
		layout,
		successfulRecoveryOperator(t),
		&fakeRecoveryStore{},
		reader,
	)
	if err != nil {
		t.Fatalf("newRecoveryWithDependencies() error = %v", err)
	}
	type classifyResult struct {
		path recoveryPath
		err  error
	}
	resultCh := make(chan classifyResult, 1)
	go func() {
		classified, classifyErr := recovery.classifyPath(t.Context(), layout.RepoDir(), recoveryPathPolicy{})
		resultCh <- classifyResult{path: classified, err: classifyErr}
	}()
	select {
	case <-reader.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not reach barrier")
	}
	retired := filepath.Join(layout.AppRoot(), "repo.reader-race")
	// A->hold 是 A/B/A 置换的第一步；lease 存活期间必须在这里失败。
	renameErr := os.Rename(layout.RepoDir(), retired)
	if !errors.Is(renameErr, windows.ERROR_SHARING_VIOLATION) &&
		!errors.Is(renameErr, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("Rename(repo while recovery reader is active) error = %v, want sharing/access denial", renameErr)
	}
	close(reader.release)
	select {
	case classified := <-resultCh:
		if classified.err != nil {
			t.Fatalf("classifyPath() error = %v", classified.err)
		}
		if classified.path.kind != recoveryPathValid || classified.path.directoryIdentity == nil {
			t.Fatalf("classifyPath() = %#v, want valid path with identity", classified.path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("classifyPath() did not finish after reader release")
	}
}

func TestService_CheckPinsRepositoryDuringReader(t *testing.T) {
	root := t.TempDir()
	layout := testServiceLayout(t, root)
	if err := os.Mkdir(layout.RepoDir(), 0o755); err != nil {
		t.Fatalf("Mkdir(repo) error = %v", err)
	}
	reader := &blockingServiceReader{
		snapshot: testServiceSnapshot("v1.0.0"),
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	service, err := newServiceWithDependencies(
		layout,
		reader,
		func(context.Context, *config.Layout) (mutationLockSet, error) {
			t.Fatal("Check() acquired mutation locks")
			return nil, nil
		},
		func(context.Context, *config.Layout, SyncRequest, OperationLogger) (syncRuntime, error) {
			t.Fatal("Check() built mutation runtime")
			return nil, nil
		},
		func(context.Context, mirror.Policy) (mirror.Plan, error) { return mirror.Plan{}, nil },
	)
	if err != nil {
		t.Fatalf("newServiceWithDependencies() error = %v", err)
	}
	type checkResult struct {
		result CheckResult
		err    error
	}
	resultCh := make(chan checkResult, 1)
	go func() {
		result, checkErr := service.Check(t.Context())
		resultCh <- checkResult{result: result, err: checkErr}
	}()
	select {
	case <-reader.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not reach barrier")
	}
	retired := filepath.Join(layout.AppRoot(), "repo.reader-race")
	// 第一步 A->hold 必须被 lease 拦截；若它成功，后续可构造 B->repo->A 的 ABA。
	renameErr := os.Rename(layout.RepoDir(), retired)
	if !errors.Is(renameErr, windows.ERROR_SHARING_VIOLATION) &&
		!errors.Is(renameErr, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("Rename(repo while Check reader is active) error = %v, want sharing/access denial", renameErr)
	}
	close(reader.release)
	select {
	case checked := <-resultCh:
		if checked.err != nil {
			t.Fatalf("Check() error = %v", checked.err)
		}
		if !checked.result.Healthy {
			t.Fatalf("Check() = %#v, want healthy result", checked.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Check() did not finish after reader release")
	}
}

func TestComponent_GitReplacementDirectoryOccupied(t *testing.T) {
	repository := newGitFixtureRepository(t,
		gitFixtureCommit{label: "old", version: "v1.0.0"},
		gitFixtureCommit{label: "new", version: "v2.0.0"},
	)
	repository.setBranch(t, "v1.0.0", "old")
	repository.setBranch(t, "v2.0.0", "new")
	server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
	source := server.source(t, "origin", "origin", true)
	plan := gitFixturePlan(t, []mirror.Source{source}, "")
	layout := componentLayout(t)
	fetcher, operator := componentFetcher(t, layout, server.caBundle)
	store := componentStateStore(t, layout)
	componentActivate(t, layout, fetcher, operator, store, plan, componentTarget(t, "v1.0.0"), componentOperationID(23))

	target := componentTarget(t, "v2.0.0")
	fetched, err := fetcher.Fetch(t.Context(), FetchRequest{
		Plan: plan, Target: target, OperationID: componentOperationID(24),
	})
	if err != nil {
		t.Fatalf("Fetch(new) error = %v", err)
	}
	tx := componentWriteUpdateTransaction(t, store, target, componentOperationID(24))
	handle := componentOpenNoDeleteHandle(t, layout.RepoDir())
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = windows.CloseHandle(handle)
		}
	})
	swapper, err := NewSwapper(layout, operator, store)
	if err != nil {
		t.Fatalf("NewSwapper() error = %v", err)
	}
	activeInspection, err := filesystem.InspectManagedDirectory(t.Context(), layout, layout.RepoDir())
	if err != nil {
		t.Fatalf("InspectManagedDirectory(active) error = %v", err)
	}
	_, err = swapper.Swap(t.Context(), SwapRequest{
		Transaction:    tx,
		Revision:       fetched.Revision,
		ActiveIdentity: activeInspection.Identity,
		UpdateIdentity: fetched.DirectoryIdentity,
		CommitEnvironment: func(context.Context, Revision) error {
			return nil
		},
	})
	assertComponentErrorCode(t, err, protocol.CodeDirectoryOccupied)
	if got, readErr := os.ReadFile(layout.RepoVersionFile()); readErr != nil || !strings.Contains(string(got), "v1.0.0") {
		t.Fatalf("old repository after occupied swap = %q, %v", got, readErr)
	}
	if closeErr := windows.CloseHandle(handle); closeErr != nil {
		t.Fatalf("CloseHandle() error = %v", closeErr)
	}
	closed = true
	removeComponentTransaction(t, store)
	removeComponentUpdate(t, operator, layout, componentOperationID(24))
	assertNoComponentTemporaryDirectories(t, layout)
	server.assertNoServerErrors(t)
}

func componentOpenNoDeleteHandle(t *testing.T, path string) windows.Handle {
	t.Helper()
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString() error = %v", err)
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile(directory) error = %v", err)
	}
	return handle
}

func componentPathIdentity(t *testing.T, path string) componentFileIdentity {
	t.Helper()
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(%q) error = %v", filepath.Base(path), err)
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile(%q) error = %v", filepath.Base(path), err)
	}
	var information windows.ByHandleFileInformation
	informationErr := windows.GetFileInformationByHandle(handle, &information)
	closeErr := windows.CloseHandle(handle)
	if informationErr != nil {
		t.Fatalf("GetFileInformationByHandle(%q) error = %v", filepath.Base(path), informationErr)
	}
	if closeErr != nil {
		t.Fatalf("CloseHandle(%q) error = %v", filepath.Base(path), closeErr)
	}
	return componentFileIdentity{
		volumeSerialNumber: information.VolumeSerialNumber,
		fileIndex: uint64(information.FileIndexHigh)<<32 |
			uint64(information.FileIndexLow),
	}
}

func componentFileAttributes(t *testing.T, path string) uint32 {
	t.Helper()
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(%q) error = %v", filepath.Base(path), err)
	}
	attributes, err := windows.GetFileAttributes(pathUTF16)
	if err != nil {
		t.Fatalf("GetFileAttributes(%q) error = %v", filepath.Base(path), err)
	}
	return attributes
}

func assertComponentReadOnlyIndex(t *testing.T, path string) {
	t.Helper()
	attributes := componentFileAttributes(t, path)
	if attributes&windows.FILE_ATTRIBUTE_READONLY == 0 {
		t.Fatalf("pack index %q attributes = %#x, want FILE_ATTRIBUTE_READONLY", filepath.Base(path), attributes)
	}
}
