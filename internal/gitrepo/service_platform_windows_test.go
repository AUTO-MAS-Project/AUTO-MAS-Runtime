//go:build windows

package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

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
