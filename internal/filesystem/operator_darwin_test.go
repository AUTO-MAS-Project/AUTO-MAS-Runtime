package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type darwinAudit struct {
	records     []DeleteAuditRecord
	startedErr  error
	finishedErr error
}

func (a *darwinAudit) RecordDeletion(_ context.Context, record DeleteAuditRecord) error {
	a.records = append(a.records, record)
	if record.Phase == DeleteAuditStarted {
		return a.startedErr
	}
	return a.finishedErr
}

func TestDarwinOperator_DeleteKinds(t *testing.T) {
	for _, kind := range []DeleteKind{DeleteUVCache, DeleteUVVersion, DeleteManagedVenv, DeleteManagedPython,
		DeleteRepositoryUpdate, DeleteRepositoryRetired, DeleteDownloadTemporary, DeleteUVStaging,
		DeletePythonCache, DeleteBuildCache, DeleteDependencySync} {
		t.Run(kind.String(), func(t *testing.T) {
			layout := darwinTestLayout(t)
			request := DeleteRequest{Kind: kind, OperationID: "operation", Reason: "test"}
			switch kind {
			case DeleteUVCache:
				request.Target = layout.UVCacheDir()
			case DeleteUVVersion:
				request.Version = "0.12.3"
				request.Target = mustDarwinPath(t)(layout.UVVersionDir(request.Version))
			case DeleteManagedVenv:
				request.Target = layout.VenvDir()
			case DeleteManagedPython:
				request.Target = layout.PythonDir()
			case DeleteRepositoryUpdate:
				request.Target = mustDarwinPath(t)(layout.RepoUpdateDir(request.OperationID))
			case DeleteRepositoryRetired:
				request.Target = mustDarwinPath(t)(layout.RepoPreviousDir(request.OperationID))
			case DeleteDownloadTemporary:
				request.Target = layout.DownloadCacheDir()
			case DeleteUVStaging:
				request.Version = "0.12.3"
				request.Target = mustDarwinPath(t)(layout.UVStagingDir(request.Version, request.OperationID))
			case DeletePythonCache:
				request.Target = filepath.Join(layout.RepoDir(), "package", "__pycache__")
			case DeleteBuildCache:
				request.Target = layout.BuildCacheDir()
			case DeleteDependencySync:
				request.Target = mustDarwinPath(t)(layout.DependencySyncDir(request.OperationID))
			}
			if err := os.MkdirAll(filepath.Join(request.Target, "nested"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(request.Target, "nested", "payload"), []byte("owned"), 0o600); err != nil {
				t.Fatal(err)
			}
			inspection, err := InspectManagedDirectory(t.Context(), layout, request.Target)
			if err != nil {
				t.Fatal(err)
			}
			request.ExpectedIdentity = inspection.Identity
			audit := &darwinAudit{}
			operator, err := New(t.Context(), layout, audit)
			if err != nil {
				t.Fatalf("New() = %v, want nil", err)
			}
			result, err := operator.RemoveTree(t.Context(), request)
			if err != nil || !result.Removed || result.Partial || !result.AuditCompleted {
				t.Fatalf("RemoveTree(%s) = %#v, %v, want complete audited removal", kind, result, err)
			}
			if len(audit.records) != 2 || audit.records[0].Phase != DeleteAuditStarted || audit.records[1].Phase != DeleteAuditFinished {
				t.Fatalf("audit = %#v, want started/finished", audit.records)
			}
			if _, err := os.Lstat(request.Target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("Lstat(removed) = %v, want absent", err)
			}
		})
	}
}

func TestDarwinOperator_ProtectedAndMismatched(t *testing.T) {
	layout := darwinTestLayout(t)
	audit := &darwinAudit{}
	operator, err := New(t.Context(), layout, audit)
	if err != nil {
		t.Fatal(err)
	}
	protected := append(layout.ProtectedRootDirs(), layout.AppRoot(), layout.RepoDir(), layout.StateDir(), t.TempDir())
	for _, target := range protected {
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(target, "keep")
		if err := os.WriteFile(sentinel, []byte("foreign"), 0o600); err != nil {
			t.Fatal(err)
		}
		result, err := operator.RemoveTree(t.Context(), DeleteRequest{Kind: DeleteBuildCache,
			Target: target, OperationID: "operation", Reason: "test"})
		var stable *Error
		if err == nil || result.Removed || !errors.As(err, &stable) || stable.Code() != protocol.CodePathOutsideManagedRoot {
			t.Fatalf("RemoveTree(%q) = %#v, %v, want boundary refusal", target, result, err)
		}
		data, err := os.ReadFile(sentinel)
		if err != nil || string(data) != "foreign" {
			t.Fatalf("sentinel = %q, %v, want preserved", data, err)
		}
	}
	if len(audit.records) != 0 {
		t.Fatalf("unauthorized audit = %#v, want none", audit.records)
	}
}

func TestDarwinOperator_UnsafeTreeIsPreserved(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			layout := darwinTestLayout(t)
			if err := os.MkdirAll(layout.BuildCacheDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			first := filepath.Join(layout.BuildCacheDir(), "a-first")
			if err := os.WriteFile(first, []byte("owned"), 0o600); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "sentinel")
			if err := os.WriteFile(outside, []byte("foreign"), 0o600); err != nil {
				t.Fatal(err)
			}
			unsafe := filepath.Join(layout.BuildCacheDir(), "z-unsafe")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(outside, unsafe)
			case "hardlink":
				err = os.Link(outside, unsafe)
			case "fifo":
				err = unix.Mkfifo(unsafe, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			operator, err := New(t.Context(), layout, &darwinAudit{})
			if err != nil {
				t.Fatal(err)
			}
			result, err := operator.RemoveTree(t.Context(), DeleteRequest{Kind: DeleteBuildCache,
				Target: layout.BuildCacheDir(), OperationID: "operation", Reason: "test"})
			if err == nil || result.Removed || result.Partial {
				t.Fatalf("RemoveTree(unsafe) = %#v, %v, want refusal before mutation", result, err)
			}
			for _, path := range []string{first, outside} {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("Stat(%q) = %v, want preserved", path, err)
				}
			}
		})
	}
}

func TestDarwinOperator_IdentityAndAudit(t *testing.T) {
	for _, scenario := range []string{"identity", "start audit", "finish audit", "missing", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			layout := darwinTestLayout(t)
			audit := &darwinAudit{}
			operator, err := New(t.Context(), layout, audit)
			if err != nil {
				t.Fatal(err)
			}
			request := DeleteRequest{Kind: DeleteBuildCache, Target: layout.BuildCacheDir(), OperationID: "operation", Reason: "test"}
			if scenario != "missing" {
				if err := os.MkdirAll(request.Target, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			ctx := t.Context()
			switch scenario {
			case "identity":
				inspection, err := InspectManagedDirectory(ctx, layout, request.Target)
				if err != nil {
					t.Fatal(err)
				}
				request.ExpectedIdentity = inspection.Identity
				if err := os.Rename(request.Target, request.Target+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(request.Target, 0o700); err != nil {
					t.Fatal(err)
				}
			case "start audit":
				audit.startedErr = errors.New("start audit error")
			case "finish audit":
				audit.finishedErr = errors.New("finish audit error")
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			result, err := operator.RemoveTree(ctx, request)
			switch scenario {
			case "identity":
				if !errors.Is(err, ErrIdentityChanged) || result.Removed {
					t.Fatalf("RemoveTree(identity) = %#v, %v, want refusal", result, err)
				}
			case "start audit":
				if !errors.Is(err, audit.startedErr) || result.Removed {
					t.Fatalf("RemoveTree(start audit) = %#v, %v, want no mutation", result, err)
				}
			case "finish audit":
				var failure *AuditError
				if !errors.As(err, &failure) || !failure.MutationApplied || !result.Removed || result.AuditCompleted {
					t.Fatalf("RemoveTree(finish audit) = %#v, %v, want applied and audit failure", result, err)
				}
			case "missing":
				if err != nil || result.Removed || !result.AuditCompleted {
					t.Fatalf("RemoveTree(missing) = %#v, %v, want audited no-op", result, err)
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) || result.Removed || len(audit.records) != 0 {
					t.Fatalf("RemoveTree(canceled) = %#v, %v, want canceled before audit", result, err)
				}
			}
		})
	}
}

func TestDarwinOperator_PartialRemovalIsReported(t *testing.T) {
	layout := darwinTestLayout(t)
	blocked := filepath.Join(layout.BuildCacheDir(), "z-blocked")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(layout.BuildCacheDir(), "a-first"), filepath.Join(blocked, "keep")} {
		if err := os.WriteFile(path, []byte("owned"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// 恢复夹具权限，确保 TempDir 能回收预期保留的失败现场。
		if err := os.Chmod(blocked, 0o700); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
		}
	})
	audit := &darwinAudit{}
	operator, err := New(t.Context(), layout, audit)
	if err != nil {
		t.Fatal(err)
	}
	result, err := operator.RemoveTree(t.Context(), DeleteRequest{Kind: DeleteBuildCache,
		Target: layout.BuildCacheDir(), OperationID: "operation", Reason: "test"})
	if err == nil || !result.Removed || !result.Partial || !result.AuditCompleted {
		t.Fatalf("RemoveTree(partial) = %#v, %v, want audited partial mutation", result, err)
	}
	if _, err := os.Stat(filepath.Join(blocked, "keep")); err != nil {
		t.Fatalf("Stat(blocked payload) = %v, want preserved failure site", err)
	}
}

func TestDarwinOperator_RenameKinds(t *testing.T) {
	for _, kind := range []RenameKind{RenameRepositoryToRetired, RenameUpdateToRepository, RenameRepositoryRollback, RenameUVStagingToVersion} {
		t.Run(kind.String(), func(t *testing.T) {
			layout := darwinTestLayout(t)
			request := darwinRenameRequest(t, layout, kind)
			if err := os.MkdirAll(request.Source, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(request.Destination), 0o700); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(request.Source)
			if err != nil {
				t.Fatal(err)
			}
			operator, err := New(t.Context(), layout, &darwinAudit{})
			if err != nil {
				t.Fatal(err)
			}
			result, err := operator.AtomicRename(t.Context(), request)
			if err != nil || !result.MutationApplied {
				t.Fatalf("AtomicRename() = %#v, %v, want applied", result, err)
			}
			after, err := os.Stat(request.Destination)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("renamed object = %v, %v, want original identity", after, err)
			}
		})
	}
}

func TestDarwinOperator_RenameNoReplaceAndIdentity(t *testing.T) {
	for _, scenario := range []string{"destination", "identity", "mismatched"} {
		t.Run(scenario, func(t *testing.T) {
			layout := darwinTestLayout(t)
			request := darwinRenameRequest(t, layout, RenameUpdateToRepository)
			if err := os.Mkdir(request.Source, 0o700); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "destination":
				if err := os.Mkdir(request.Destination, 0o700); err != nil {
					t.Fatal(err)
				}
			case "identity":
				inspection, err := InspectManagedDirectory(t.Context(), layout, request.Source)
				if err != nil {
					t.Fatal(err)
				}
				request.ExpectedSourceIdentity = inspection.Identity
				if err := os.Rename(request.Source, request.Source+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(request.Source, 0o700); err != nil {
					t.Fatal(err)
				}
			case "mismatched":
				request.Destination = layout.DataDir()
			}
			operator, err := New(t.Context(), layout, &darwinAudit{})
			if err != nil {
				t.Fatal(err)
			}
			result, err := operator.AtomicRename(t.Context(), request)
			if err == nil || result.MutationApplied {
				t.Fatalf("AtomicRename(%s) = %#v, %v, want refusal", scenario, result, err)
			}
			if _, err := os.Stat(request.Source); err != nil {
				t.Fatalf("Stat(source) = %v, want preserved", err)
			}
		})
	}
}

func mustDarwinPath(t *testing.T) func(string, error) string {
	t.Helper()
	return func(path string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
}

func darwinRenameRequest(t *testing.T, layout *config.Layout, kind RenameKind) RenameRequest {
	t.Helper()
	request := RenameRequest{Kind: kind, OperationID: "operation", Reason: "test"}
	switch kind {
	case RenameRepositoryToRetired:
		request.Source = layout.RepoDir()
		request.Destination = mustDarwinPath(t)(layout.RepoPreviousDir(request.OperationID))
	case RenameUpdateToRepository:
		request.Source = mustDarwinPath(t)(layout.RepoUpdateDir(request.OperationID))
		request.Destination = layout.RepoDir()
	case RenameRepositoryRollback:
		request.Source = mustDarwinPath(t)(layout.RepoPreviousDir(request.OperationID))
		request.Destination = layout.RepoDir()
	case RenameUVStagingToVersion:
		request.Version = "0.12.3"
		request.Source = mustDarwinPath(t)(layout.UVStagingDir(request.Version, request.OperationID))
		request.Destination = mustDarwinPath(t)(layout.UVVersionDir(request.Version))
	}
	return request
}
