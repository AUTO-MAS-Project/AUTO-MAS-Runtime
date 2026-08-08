package uv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestDependencies_LockfileContract(t *testing.T) {
	tests := []struct {
		name     string
		lockKind string
		wantCode protocol.Code
	}{
		{name: "missing", lockKind: "missing", wantCode: protocol.CodeLockfileMissing},
		{name: "directory", lockKind: "directory", wantCode: protocol.CodeLockfileMissing},
		{name: "outdated", lockKind: "outdated", wantCode: protocol.CodeLockfileOutdated},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			layout, err := config.NewLayout(root, filepath.Dir(root))
			if err != nil {
				t.Fatalf("NewLayout() error = %v", err)
			}
			runner := &fakeDependenciesRunner{}
			lockPath := layout.UVLockFile()
			switch test.lockKind {
			case "directory":
				if err := os.MkdirAll(lockPath, 0o700); err != nil {
					t.Fatalf("MkdirAll() error = %v", err)
				}
			case "outdated":
				writeLockfile(t, lockPath)
				runner.responses = []fakeRunnerResponse{{result: UVResult{ExitCode: 1}, err: errors.New("lock is stale")}}
			}
			service, err := NewDependenciesService(layout, runner, &fakeTreeRemover{})
			if err != nil {
				t.Fatalf("NewDependenciesService() error = %v", err)
			}
			_, err = service.Check(context.Background(), dependencyTestRequest(layout))
			assertPythonCode(t, err, test.wantCode)
		})
	}
}

func TestDependencies_SyncArguments(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	writeLockfile(t, layout.UVLockFile())
	runner := &fakeDependenciesRunner{
		responses: []fakeRunnerResponse{
			{result: UVResult{}},
			{result: UVResult{}},
		},
	}
	service, err := NewDependenciesService(layout, runner, &fakeTreeRemover{})
	if err != nil {
		t.Fatalf("NewDependenciesService() error = %v", err)
	}
	result, err := service.Sync(context.Background(), dependencyTestRequest(layout))
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if !result.Synchronized || !result.LockfileChecked {
		t.Fatalf("Sync() result = %#v, want checked and synchronized", result)
	}
	want := []string{
		"sync", "--project", layout.RepoDir(), "--python", "3.12.10",
		"--locked", "--no-default-groups", "--no-install-workspace",
	}
	if got := runner.calls[1].args; !reflect.DeepEqual(got, want) {
		t.Fatalf("sync args = %#v, want %#v", got, want)
	}
}

func TestDependencies_RebuildUsesControlledDelete(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	remover := &fakeTreeRemover{result: filesystem.DeleteResult{Removed: true}}
	service, err := NewDependenciesService(layout, &fakeDependenciesRunner{}, remover)
	if err != nil {
		t.Fatalf("NewDependenciesService() error = %v", err)
	}
	result, err := service.Rebuild(context.Background(), dependencyTestRequest(layout))
	if err != nil {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if !result.Rebuilt {
		t.Fatalf("Rebuild() result = %#v, want rebuilt", result)
	}
	if got, want := remover.request.Kind, filesystem.DeleteManagedVenv; got != want {
		t.Fatalf("delete kind = %q, want %q", got, want)
	}
	if got, want := remover.request.Target, layout.VenvDir(); got != want {
		t.Fatalf("delete target = %q, want %q", got, want)
	}
}

type fakeRunnerResponse struct {
	result UVResult
	err    error
}

type fakeDependenciesCall struct {
	args    []string
	options RunOptions
}

type fakeDependenciesRunner struct {
	responses []fakeRunnerResponse
	calls     []fakeDependenciesCall
}

func (r *fakeDependenciesRunner) Run(_ context.Context, args []string, options RunOptions) (UVResult, error) {
	r.calls = append(r.calls, fakeDependenciesCall{
		args:    append([]string(nil), args...),
		options: options,
	})
	if len(r.responses) == 0 {
		return UVResult{}, nil
	}
	response := r.responses[0]
	r.responses = r.responses[1:]
	return response.result, response.err
}

type fakeTreeRemover struct {
	request filesystem.DeleteRequest
	result  filesystem.DeleteResult
	err     error
}

func (r *fakeTreeRemover) RemoveTree(_ context.Context, request filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
	r.request = request
	return r.result, r.err
}

func dependencyTestRequest(layout *config.Layout) DependenciesRequest {
	return DependenciesRequest{
		ProjectDir:    layout.RepoDir(),
		ProjectEnvDir: layout.VenvDir(),
		PythonVersion: "3.12.10",
		OperationID:   "01J00000000000000000000000",
		Branch:        "release/v5.4.0",
		Commit:        "0123456789abcdef0123456789abcdef01234567",
	}
}

func writeLockfile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte("version = 1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}
