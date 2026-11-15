//go:build windows

package gitrepo

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestRecovery_ClassifyPathPinsRepositoryDuringReader(t *testing.T) {
	layout := mustGitLayout(t)
	writeRecoveryRepository(t, layout.RepoDir(), "v5.4.0", recoverySourceURL(t), "active")
	snapshot, err := (goGitRepositoryReader{}).Inspect(t.Context(), layout.RepoDir())
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
