package gitrepo

import (
	"context"
	"errors"
	"syscall"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestSwapper_OperationDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name    string
		cleanup bool
	}{{name: "occupied rename"}, {name: "committed cleanup", cleanup: true}} {
		t.Run(test.name, func(t *testing.T) {
			layout := mustGitLayout(t)
			request := validSwapRequest(t, layout, "01ARZ3NDEKTSV4RRFFQ69G5FAZ")
			writeSwapMarker(t, layout.RepoDir(), "old-only", "old")
			writeSwapMarker(t, mustRepoUpdateDir(t, layout, request.Transaction.OperationID), "new-only", "new")
			operator := successfulSwapOperator(t)
			if test.cleanup {
				operator.remove = func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
					return filesystem.DeleteResult{Partial: true, AuditCompleted: true}, &filesystem.FileError{Operation: "remove-tree", Path: "private-path", Err: syscall.Errno(5)}
				}
			} else {
				operator.rename = func(context.Context, filesystem.RenameRequest) (filesystem.RenameResult, error) {
					return filesystem.RenameResult{}, errors.Join(testSwapCodeError{code: protocol.CodeDirectoryOccupied}, &filesystem.FileError{Operation: "rename", Path: "private-path", Err: syscall.Errno(32)})
				}
			}
			_, err := mustTestSwapper(t, layout, operator, &fakeSwapTransactionWriter{}).Swap(t.Context(), request)
			var failure *Error
			if !errors.As(err, &failure) {
				t.Fatalf("error = %v, want repository error", err)
			}
			if len(failure.Details()) != 0 {
				t.Fatalf("protocol details = %#v, want unchanged empty map", failure.Details())
			}
			diagnostic, ok := any(failure).(interface{ DiagnosticDetails() map[string]any })
			if !ok {
				t.Fatal("repository error has no diagnostics")
			}
			details, ok := diagnostic.DiagnosticDetails()["repository"].(map[string]any)
			if !ok {
				t.Fatalf("diagnostics = %#v, want repository facts", diagnostic.DiagnosticDetails())
			}
			if test.cleanup {
				if details["component"] != "previous" || details["partial"] != true || details["auditCompleted"] != true || details["committed"] != true || details["removed"] != false {
					t.Fatalf("cleanup diagnostics = %#v, want original cleanup and activation facts", details)
				}
			} else if details["sourceRole"] != "repo" || details["destinationRole"] != "previous" || details["phase"] != "repository_to_retired" || details["mutationApplied"] != false {
				t.Fatalf("rename diagnostics = %#v, want exact failed rename", details)
			}
			primary, ok := details["primary"].(map[string]any)
			if !ok || primary["windowsError"] == nil {
				t.Fatalf("primary = %#v, want system error", primary)
			}
		})
	}
}

func TestFetcher_CleanupDiagnostics(t *testing.T) {
	target := mustParseTarget(t, "v5.4.0")
	client := &fakeGitClient{
		list: func(context.Context, string, []byte) ([]*plumbing.Reference, error) {
			return targetBranchReferences(target), nil
		},
		clone: func(context.Context, string, git.CloneOptions) error { return errTestClone },
	}
	deps := successfulFetcherDependencies(t, client)
	deps.remover = &fakeTreeRemover{remove: func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
		return filesystem.DeleteResult{Partial: true, AuditCompleted: true}, &filesystem.FileError{Operation: "remove-tree", Path: "private-path", Err: syscall.Errno(5)}
	}}
	_, err := mustTestFetcher(t, deps).Fetch(t.Context(), FetchRequest{Plan: mustGitPlan(t, "cnb"), Target: target, OperationID: "OPERATION-CLEANUP-DIAGNOSTICS"})
	assertGitrepoCode(t, err, protocol.CodeGitRepoCleanupFailed)
	var diagnostic interface{ DiagnosticDetails() map[string]any }
	if !errors.As(err, &diagnostic) {
		t.Fatal("fetch failure has no cleanup diagnostics")
	}
	repository, ok := diagnostic.DiagnosticDetails()["repository"].(map[string]any)
	if !ok || repository["component"] != "update" || repository["partial"] != true || repository["committed"] != false {
		t.Fatalf("diagnostics = %#v, want update cleanup facts on final error", diagnostic.DiagnosticDetails())
	}
}
