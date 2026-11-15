//go:build windows

package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestComponent_GitReplacementMatrix(t *testing.T) {
	t.Run("first installation", func(t *testing.T) {
		repository := newGitFixtureRepository(t, gitFixtureCommit{label: "target", version: "v1.0.0"})
		repository.setBranch(t, "v1.0.0", "target")
		server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
		source := server.source(t, "origin", "origin", true)
		plan := gitFixturePlan(t, []mirror.Source{source}, "")
		layout := componentLayout(t)
		fetcher, operator := componentFetcher(t, layout, server.caBundle)
		store := componentStateStore(t, layout)
		target := componentTarget(t, "v1.0.0")

		fetched := componentActivate(t, layout, fetcher, operator, store, plan, target, componentOperationID(20))
		assertFetchedRepositoryShape(t, layout.RepoDir(), target, source, fetched.Revision.Commit())
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})

	t.Run("dirty tracked and untracked files are replaced", func(t *testing.T) {
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
		oldTarget := componentTarget(t, "v1.0.0")
		newTarget := componentTarget(t, "v2.0.0")

		old := componentActivate(t, layout, fetcher, operator, store, plan, oldTarget, componentOperationID(21))
		tracked := filepath.Join(layout.RepoDir(), "res", "version.json")
		if err := os.WriteFile(tracked, []byte(`{"version":"v1.0.0","dirty":true}`), 0o600); err != nil {
			t.Fatalf("WriteFile(tracked) error = %v", err)
		}
		untracked := filepath.Join(layout.RepoDir(), "untracked-dirty.txt")
		if err := os.WriteFile(untracked, []byte("local change"), 0o600); err != nil {
			t.Fatalf("WriteFile(untracked) error = %v", err)
		}

		newRevision := componentActivate(t, layout, fetcher, operator, store, plan, newTarget, componentOperationID(22))
		if newRevision.Revision.Commit() == old.Revision.Commit() {
			t.Fatal("replacement kept the old commit")
		}
		assertFetchedRepositoryShape(t, layout.RepoDir(), newTarget, source, newRevision.Revision.Commit())
		if _, err := os.Lstat(untracked); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("untracked file remains after replacement: %v", err)
		}
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})

	t.Run("real read-only pack index is removed from retired repository", func(t *testing.T) {
		repository := newGitFixtureRepository(t, gitFixtureCommit{label: "target", version: "v1.0.0"})
		repository.setBranch(t, "v1.0.0", "target")
		server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
		source := server.source(t, "origin", "origin", true)
		layout := componentLayout(t)
		operator := componentOperator(t, layout)
		target := componentTarget(t, "v1.0.0")
		operationID := componentOperationID(36)
		previous := mustRepoPreviousDir(t, layout, operationID)

		_, err := git.PlainCloneContext(t.Context(), previous, false, &git.CloneOptions{
			URL:               source.BaseURL(),
			ReferenceName:     plumbing.NewBranchReferenceName(target.Branch()),
			SingleBranch:      true,
			Depth:             1,
			Tags:              git.NoTags,
			RecurseSubmodules: git.NoRecurseSubmodules,
			CABundle:          append([]byte(nil), server.caBundle...),
		})
		if err != nil {
			t.Fatalf("PlainCloneContext() error = %v", err)
		}
		indexes, err := filepath.Glob(filepath.Join(previous, ".git", "objects", "pack", "*.idx"))
		if err != nil {
			t.Fatalf("Glob(pack indexes) error = %v", err)
		}
		if len(indexes) == 0 {
			t.Fatal("PlainCloneContext() produced no pack index")
		}
		for _, index := range indexes {
			attributes := componentFileAttributes(t, index)
			if attributes&windows.FILE_ATTRIBUTE_READONLY == 0 {
				t.Fatalf("pack index %q attributes = %#x, want FILE_ATTRIBUTE_READONLY", filepath.Base(index), attributes)
			}
		}

		result, err := operator.RemoveTree(t.Context(), filesystem.DeleteRequest{
			Kind:        filesystem.DeleteRepositoryRetired,
			Target:      previous,
			OperationID: operationID,
			Reason:      "component-real-pack-cleanup",
		})
		if err != nil {
			t.Fatalf("RemoveTree(retired) error = %v", err)
		}
		if result.Partial || !result.AuditCompleted {
			t.Fatalf("RemoveTree(retired) result = %#v, want complete audit", result)
		}
		if _, err := os.Lstat(previous); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retired repository remains after cleanup: %v", err)
		}
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})

	t.Run("protected directories survive fetch swap and cleanup", func(t *testing.T) {
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
		protected := seedComponentProtectedDirectories(t, layout)
		fetcher, operator := componentFetcher(t, layout, server.caBundle)
		store := componentStateStore(t, layout)

		componentActivate(
			t,
			layout,
			fetcher,
			operator,
			store,
			plan,
			componentTarget(t, "v1.0.0"),
			componentOperationID(37),
		)
		assertComponentProtectedDirectories(t, protected)
		activated := componentActivate(
			t,
			layout,
			fetcher,
			operator,
			store,
			plan,
			componentTarget(t, "v2.0.0"),
			componentOperationID(38),
		)
		if activated.Revision.Commit() != repository.hash(t, "new").String() {
			t.Fatalf("activated commit = %q, want new commit", activated.Revision.Commit())
		}
		assertComponentProtectedDirectories(t, protected)
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})

	t.Run("directory handle without delete share maps to occupied", func(t *testing.T) {
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
	})

	t.Run("failed verification preserves complete active repository", func(t *testing.T) {
		repository := newGitFixtureRepository(t,
			gitFixtureCommit{label: "old", version: "v1.0.0"},
			gitFixtureCommit{label: "wrong", version: "v1.0.0"},
		)
		repository.setBranch(t, "v1.0.0", "old")
		repository.setBranch(t, "v2.0.0", "wrong")
		server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
		source := server.source(t, "origin", "origin", true)
		plan := gitFixturePlan(t, []mirror.Source{source}, "")
		layout := componentLayout(t)
		fetcher, operator := componentFetcher(t, layout, server.caBundle)
		store := componentStateStore(t, layout)
		oldTarget := componentTarget(t, "v1.0.0")
		componentActivate(t, layout, fetcher, operator, store, plan, oldTarget, componentOperationID(25))

		_, err := fetcher.Fetch(t.Context(), FetchRequest{
			Plan: plan, Target: componentTarget(t, "v2.0.0"), OperationID: componentOperationID(26),
		})
		assertComponentErrorCode(t, err, protocol.CodeGitVersionMismatch)
		assertFetchedRepositoryShape(t, layout.RepoDir(), oldTarget, source, repository.hash(t, "old").String())
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})
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

func seedComponentProtectedDirectories(
	t *testing.T,
	layout *config.Layout,
) map[string]componentProtectedSnapshot {
	t.Helper()
	paths := []string{
		layout.ConfigDir(),
		layout.DataDir(),
		layout.HistoryDir(),
		layout.ScriptDir(),
		layout.DebugDir(),
		layout.PluginsDir(),
		layout.LogsDir(),
		layout.RuntimeLogDir(),
		layout.RuntimeDir(),
	}
	snapshots := make(map[string]componentProtectedSnapshot, len(paths))
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", filepath.Base(path), err)
		}
		sentinel := filepath.Join(path, "m4-protected-sentinel.txt")
		content := "protected:" + filepath.Base(path)
		if err := os.WriteFile(sentinel, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", filepath.Base(path), err)
		}
		snapshots[path] = componentProtectedSnapshot{
			directory: componentPathIdentity(t, path),
			sentinel:  componentPathIdentity(t, sentinel),
			content:   content,
		}
	}
	return snapshots
}

func assertComponentProtectedDirectories(
	t *testing.T,
	want map[string]componentProtectedSnapshot,
) {
	t.Helper()
	for path, snapshot := range want {
		directory := componentPathIdentity(t, path)
		if directory != snapshot.directory {
			t.Fatalf("protected directory %q identity = %#v, want %#v", filepath.Base(path), directory, snapshot.directory)
		}
		sentinelPath := filepath.Join(path, "m4-protected-sentinel.txt")
		sentinel := componentPathIdentity(t, sentinelPath)
		if sentinel != snapshot.sentinel {
			t.Fatalf("protected sentinel %q identity = %#v, want %#v", filepath.Base(path), sentinel, snapshot.sentinel)
		}
		content, err := os.ReadFile(sentinelPath)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", filepath.Base(path), err)
		}
		if string(content) != snapshot.content {
			t.Fatalf("protected sentinel %q content = %q, want %q", filepath.Base(path), content, snapshot.content)
		}
	}
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
