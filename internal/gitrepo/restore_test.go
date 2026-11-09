package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

func sourceRestoreFixture(t *testing.T) (*config.Layout, string) {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainInit(layout.RepoDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"uv.lock": "version = 1\n", "app/main.py": "original\n", "frontend/keep.txt": "excluded\n", "res/version.json": `{"version":"v1.0.0"}`} {
		path := filepath.Join(layout.RepoDir(), filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	hash, err := wt.Commit("source fixture", &git.CommitOptions{Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Unix(1, 0)}})
	if err != nil {
		t.Fatal(err)
	}
	branch := plumbing.NewBranchReferenceName("release/v1.0.0")
	if err := repo.Storer.SetReference(plumbing.NewHashReference(branch, hash)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branch)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetShallow([]plumbing.Hash{hash}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{"https://github.com/AUTO-MAS-Project/AUTO-MAS.git"}}); err != nil {
		t.Fatal(err)
	}
	return layout, hash.String()
}

func TestRestoreTrackedFiles_LocalCommit(t *testing.T) {
	layout, commit := sourceRestoreFixture(t)
	if err := os.WriteFile(layout.UVLockFile(), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainOpen(layout.RepoDir())
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("uv.lock"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(layout.RepoDir(), "app", "main.py")); err != nil {
		t.Fatal(err)
	}
	untracked := filepath.Join(layout.RepoDir(), "untracked.txt")
	if err := os.WriteFile(untracked, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	excluded := filepath.Join(layout.RepoDir(), "frontend", "keep.txt")
	if err := os.WriteFile(excluded, []byte("keep modified"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeCalls := 0
	count, err := (goGitRepositoryReader{}).Restore(t.Context(), layout, commit, func() error { beforeCalls++; return nil })
	if err != nil || count != 2 || beforeCalls != 1 {
		t.Fatalf("Restore() = %d, %v, before=%d, want 2, nil, 1", count, err, beforeCalls)
	}
	for name, want := range map[string]string{"uv.lock": "version = 1\n", "app/main.py": "original\n", "untracked.txt": "keep", "frontend/keep.txt": "keep modified"} {
		got, err := os.ReadFile(filepath.Join(layout.RepoDir(), filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Fatalf("file %s = %q, %v, want %q", name, got, err, want)
		}
	}
	count, err = (goGitRepositoryReader{}).Restore(t.Context(), layout, commit, func() error { t.Fatal("clean restore wrote state"); return nil })
	if err != nil || count != 0 {
		t.Fatalf("second restore = %d, %v", count, err)
	}
}

func TestRestoreTrackedFiles_BeforeWriteFailure(t *testing.T) {
	layout, commit := sourceRestoreFixture(t)
	if err := os.WriteFile(layout.UVLockFile(), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("state write rejected")
	_, err := (goGitRepositoryReader{}).Restore(t.Context(), layout, commit, func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want state rejection", err)
	}
	got, err := os.ReadFile(layout.UVLockFile())
	if err != nil || string(got) != "broken" {
		t.Fatalf("source after rejection = %q, %v", got, err)
	}
}

func TestService_SyncRestoresTrackedFiles(t *testing.T) {
	for _, useCurrent := range []bool{true, false} {
		t.Run(map[bool]string{true: "early_noop", false: "fetched_same_commit"}[useCurrent], func(t *testing.T) {
			layout, commit := sourceRestoreFixture(t)
			if err := os.WriteFile(layout.UVLockFile(), []byte("broken"), 0o600); err != nil {
				t.Fatal(err)
			}
			runtime := newServiceTestRuntime()
			runtime.environment = state.EnvironmentState{SchemaVersion: state.SchemaVersion, Status: protocol.StateReadyToStart, LastSuccessful: state.Revision{Version: "v1.0.0", Commit: commit}}
			service := newTestService(t, layout, goGitRepositoryReader{}, runtime, nil)
			revision, err := NewRevision("v1.0.0", "release/v1.0.0", commit, "github")
			if err != nil {
				t.Fatal(err)
			}
			runtime.fetchResult = FetchResult{Revision: revision}
			service.resolveRemote = func(context.Context, mirror.Plan, Target) (string, error) { return strings.Repeat("a", 40), nil }
			request := testSyncRequest(mustServiceTarget(t, "v1.0.0"))
			request.UseCurrent = useCurrent
			result, err := service.Sync(t.Context(), request)
			if err != nil || !result.Changed || result.Status != protocol.StateEnvironmentBroken {
				t.Fatalf("Sync() = %#v, %v, want restored broken", result, err)
			}
			wantFetch := 0
			if !useCurrent {
				wantFetch = 1
			}
			if runtime.fetchCalls != wantFetch || runtime.swapCalls != 0 || runtime.writeEnvironmentCalls != 1 {
				t.Fatalf("side effects fetch=%d swap=%d writes=%d", runtime.fetchCalls, runtime.swapCalls, runtime.writeEnvironmentCalls)
			}
		})
	}
}

func TestRestoreTrackedFiles_RejectsMissingBlobAndDirectory(t *testing.T) {
	for _, kind := range []string{"missing_blob", "directory"} {
		t.Run(kind, func(t *testing.T) {
			layout, commit := sourceRestoreFixture(t)
			if kind == "directory" {
				if err := os.Remove(layout.UVLockFile()); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(layout.UVLockFile(), 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				repo, err := git.PlainOpen(layout.RepoDir())
				if err != nil {
					t.Fatal(err)
				}
				obj, err := repo.CommitObject(plumbing.NewHash(commit))
				if err != nil {
					t.Fatal(err)
				}
				file, err := obj.File("uv.lock")
				if err != nil {
					t.Fatal(err)
				}
				hash := file.Hash.String()
				if err := os.Remove(filepath.Join(layout.RepoDir(), ".git", "objects", hash[:2], hash[2:])); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(layout.UVLockFile(), []byte("broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := (goGitRepositoryReader{}).Restore(t.Context(), layout, commit, func() error { t.Fatal("invalid input began restoration"); return nil })
			if err == nil {
				t.Fatal("Restore accepted invalid input")
			}
		})
	}
}

func TestRestoreTrackedFiles_CancelBeforeReplace(t *testing.T) {
	layout, commit := sourceRestoreFixture(t)
	if err := os.WriteFile(layout.UVLockFile(), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := (goGitRepositoryReader{}).Restore(ctx, layout, commit, func() error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancelled", err)
	}
	got, err := os.ReadFile(layout.UVLockFile())
	if err != nil || string(got) != "broken" {
		t.Fatalf("cancelled source = %q, %v", got, err)
	}
	count, err := (goGitRepositoryReader{}).Restore(t.Context(), layout, commit, func() error { return nil })
	if err != nil || count != 1 {
		t.Fatalf("resumed restore = %d, %v", count, err)
	}
}
