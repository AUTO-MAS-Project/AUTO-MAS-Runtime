package gitrepo

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-billy/v5"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

func TestDarwinGitFS_RejectsEscapeAndLinks(t *testing.T) {
	access := newDarwinGitFixture(t)
	files, err := openGitFiles(t.Context(), access)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := files.close(); err != nil {
			t.Error(err)
		}
	}()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "keep")
	if err := os.WriteFile(sentinel, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(access.path, "parent-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, filepath.Join(access.path, "file-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(sentinel, filepath.Join(access.path, "hard-link")); err != nil {
		t.Fatal(err)
	}
	worktree := files.worktree
	child, err := worktree.Chroot("child")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		name string
		view billy.Filesystem
		path string
	}{
		{name: "absolute", view: worktree, path: sentinel},
		{name: "traversal", view: worktree, path: "one/../target"},
		{name: "joined traversal", view: worktree, path: worktree.Join("child", "..", "target")},
		{name: "chroot traversal", view: child, path: "../target"},
		{name: "symlink ancestor", view: worktree, path: "parent-link/keep"},
		{name: "symlink leaf", view: worktree, path: "file-link"},
		{name: "hard link", view: worktree, path: "hard-link"},
		{name: "metadata alias", view: worktree, path: ".GiT/config"},
	} {
		t.Run(input.name, func(t *testing.T) {
			created, err := input.view.Create(input.path)
			if created != nil || err == nil {
				if created != nil {
					t.Error(created.Close())
				}
				t.Fatalf("Create(%q) = %v, %v, want refusal", input.path, created, err)
			}
		})
	}
	if _, err := child.Chroot("../"); !errors.Is(err, billy.ErrCrossedBoundary) {
		t.Fatalf("Chroot(../) = %v, want boundary error", err)
	}
	if err := worktree.Symlink(sentinel, "new-link"); !errors.Is(err, billy.ErrNotSupported) {
		t.Fatalf("Symlink() = %v, want unsupported", err)
	}
	if _, err := worktree.ReadDir("."); err == nil {
		t.Fatal("ReadDir accepted a directory with unsafe entries")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "original" {
		t.Fatalf("outside file = %q, %v, want original", data, err)
	}
	if _, err := os.Lstat(filepath.Join(access.path, "target")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target after rejection = %v, want absent", err)
	}
}

func TestDarwinGitFS_RetainsOriginalRoot(t *testing.T) {
	access := newDarwinGitFixture(t)
	files, err := openGitFiles(t.Context(), access)
	if err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(filepath.Dir(access.path), "moved")
	if err := os.Rename(access.path, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(access.path, 0o700); err != nil {
		t.Fatal(err)
	}
	newLeaf := filepath.Join(access.path, "sentinel")
	if err := os.WriteFile(newLeaf, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := files.worktree.Create("sentinel")
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write([]byte("authorized"))
	if err := errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	if err := files.close(); !errors.Is(err, filesystem.ErrIdentityChanged) {
		t.Fatalf("close with replaced root = %v, want identity rejection", err)
	}
	for _, result := range []struct{ path, want string }{
		{path: newLeaf, want: "replacement"},
		{path: filepath.Join(saved, "sentinel"), want: "authorized"},
	} {
		if data, err := os.ReadFile(result.path); err != nil || string(data) != result.want {
			t.Fatalf("ReadFile(%q) = %q, %v, want %q", result.path, data, err, result.want)
		}
	}
}

func TestDarwinGitFS_ClosesAndCancels(t *testing.T) {
	access := newDarwinGitFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	files, err := openGitFiles(ctx, access)
	if err != nil {
		t.Fatal(err)
	}
	file, err := files.metadata.TempFile("", "pack-")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(file.Name()) || filepath.Dir(file.Name()) != "." {
		t.Fatalf("TempFile name = %q, want relative metadata leaf", file.Name())
	}
	cancel()
	if _, err := files.metadata.Open(file.Name()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open after cancellation = %v, want context.Canceled", err)
	}
	if err := files.close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("close after cancellation = %v, want context.Canceled", err)
	}
	if _, err := file.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("file retained after close: read = %v, want os.ErrClosed", err)
	}
	if _, err := files.worktree.Stat("."); !errors.Is(err, filesystem.ErrClosed) {
		t.Fatalf("closed worktree Stat = %v, want ErrClosed", err)
	}
}

func TestDarwinGitFS_CloneAndInspect(t *testing.T) {
	sourceRepo := newGitFixtureRepository(t,
		gitFixtureCommit{label: "base", version: "v0.9.0"},
		gitFixtureCommit{label: "head", version: "v1.0.0", files: map[string]string{
			"app/keep": "application", "frontend/skip": "not installed",
		}},
	)
	sourceRepo.setBranch(t, "v1.0.0", "head")
	server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": sourceRepo})
	source := server.source(t, "origin", "origin", true)
	plan := gitFixturePlan(t, []mirror.Source{source}, "")
	layout := componentLayout(t)
	fetcher, operator := componentFetcher(t, layout, server.caBundle)
	target := componentTarget(t, "v1.0.0")
	operationID := componentOperationID(210)
	result, err := fetcher.Fetch(t.Context(), FetchRequest{Plan: plan, Target: target, OperationID: operationID})
	if err != nil {
		t.Fatalf("Fetch = %v; server errors: %v", err, errors.Join(server.snapshotErrors()...))
	}
	if result.Revision.Commit() != sourceRepo.hash(t, "head").String() {
		t.Fatalf("revision = %v, want fixture head", result.Revision)
	}
	assertFetchedRepositoryShape(t, result.RepositoryPath, target, source, result.Revision.Commit())
	assertSparseCheckoutPath(t, result.RepositoryPath, "app/keep", true, "application")
	assertSparseCheckoutPath(t, result.RepositoryPath, "frontend", false, "")
	files, err := openGitFiles(t.Context(), testRepositoryAccess(t, result.RepositoryPath))
	if err != nil {
		t.Fatal(err)
	}
	item, err := files.worktree.Open("app/keep")
	if err != nil {
		t.Fatal(errors.Join(err, files.close()))
	}
	data, readErr := io.ReadAll(item)
	if err := errors.Join(readErr, item.Close(), files.close()); err != nil || string(data) != "application" {
		t.Fatalf("rooted file = %q, %v, want application", data, err)
	}
	removeComponentUpdate(t, operator, layout, operationID)
	assertNoComponentTemporaryDirectories(t, layout)
	server.assertNoServerErrors(t)
}

func newDarwinGitFixture(t *testing.T) repositoryAccess {
	t.Helper()
	layout := componentLayout(t)
	if err := os.Mkdir(layout.RepoDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	return testRepositoryAccess(t, layout.RepoDir())
}
