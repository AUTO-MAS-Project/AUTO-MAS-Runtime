package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

func TestSwapper_ReplacesRepositoryAndDeletesRetiredTree(t *testing.T) {
	layout := mustGitLayout(t)
	request := validSwapRequest(t, layout, "01ARZ3NDEKTSV4RRFFQ69G5FAV")
	writeSwapMarker(t, layout.RepoDir(), "old-only", "old")
	writeSwapMarker(t, mustRepoUpdateDir(t, layout, request.Transaction.OperationID), "new-only", "new")
	operator := successfulSwapOperator(t)
	transactions := &fakeSwapTransactionWriter{}
	swapper := mustTestSwapper(t, layout, operator, transactions)

	result, err := swapper.Swap(t.Context(), request)
	if err != nil {
		t.Fatalf("Swap() error = %v", err)
	}
	if !result.MutationApplied || !result.RepositoryActivated || !result.CleanupCompleted {
		t.Fatalf("Swap() result = %#v, want applied/active/clean", result)
	}
	if result.Revision != request.Revision {
		t.Fatalf("Swap() revision = %#v, want request revision", result.Revision)
	}
	if got, err := os.ReadFile(filepath.Join(layout.RepoDir(), "new-only")); err != nil || string(got) != "new" {
		t.Fatalf("active marker = %q, %v, want new", got, err)
	}
	if _, err := os.Lstat(filepath.Join(layout.RepoDir(), "old-only")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old marker remains: %v", err)
	}
	previous := mustRepoPreviousDir(t, layout, request.Transaction.OperationID)
	if _, err := os.Lstat(previous); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous directory remains: %v", err)
	}
	if got, want := transactions.stages, []protocol.Stage{protocol.StageWorkspaceSwap, protocol.StageWorkspaceCleanup}; !equalStages(got, want) {
		t.Fatalf("transaction stages = %v, want %v", got, want)
	}
}

func TestSwapper_FirstInstallationSkipsRetiredCleanup(t *testing.T) {
	layout := mustGitLayout(t)
	request := validSwapRequest(t, layout, "01ARZ3NDEKTSV4RRFFQ69G5FA2")
	writeSwapMarker(t, mustRepoUpdateDir(t, layout, request.Transaction.OperationID), "new-only", "new")
	operator := successfulSwapOperator(t)
	operator.remove = func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
		t.Fatal("RemoveTree() called during first installation")
		return filesystem.DeleteResult{}, nil
	}
	swapper := mustTestSwapper(t, layout, operator, &fakeSwapTransactionWriter{})

	result, err := swapper.Swap(t.Context(), request)
	if err != nil {
		t.Fatalf("Swap() error = %v", err)
	}
	if !result.MutationApplied || !result.RepositoryActivated || !result.CleanupCompleted {
		t.Fatalf("Swap() result = %#v, want first installation committed", result)
	}
	assertSwapMarker(t, filepath.Join(layout.RepoDir(), "new-only"), "new")
}

func TestSwapper_PreexistingPreviousFailsClosed(t *testing.T) {
	layout := mustGitLayout(t)
	request := validSwapRequest(t, layout, "01ARZ3NDEKTSV4RRFFQ69G5FA3")
	update := mustRepoUpdateDir(t, layout, request.Transaction.OperationID)
	previous := mustRepoPreviousDir(t, layout, request.Transaction.OperationID)
	writeSwapMarker(t, update, "new-only", "new")
	writeSwapMarker(t, previous, "foreign", "foreign")
	operator := successfulSwapOperator(t)
	operator.rename = func(context.Context, filesystem.RenameRequest) (filesystem.RenameResult, error) {
		t.Fatal("AtomicRename() called with preexisting previous directory")
		return filesystem.RenameResult{}, nil
	}
	operator.remove = func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
		t.Fatal("RemoveTree() called with preexisting previous directory")
		return filesystem.DeleteResult{}, nil
	}
	swapper := mustTestSwapper(t, layout, operator, &fakeSwapTransactionWriter{})

	result, err := swapper.Swap(t.Context(), request)
	assertGitrepoCode(t, err, protocol.CodeUpdateStateAmbiguous)
	if result.MutationApplied {
		t.Fatalf("Swap() result = %#v, want no mutation", result)
	}
	assertSwapMarker(t, filepath.Join(update, "new-only"), "new")
	assertSwapMarker(t, filepath.Join(previous, "foreign"), "foreign")
	if _, err := os.Lstat(layout.RepoDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repo created despite ambiguous previous: %v", err)
	}
}

func TestSwapper_FirstRenameFailureLeavesStateRecoverable(t *testing.T) {
	layout := mustGitLayout(t)
	request := validSwapRequest(t, layout, "01ARZ3NDEKTSV4RRFFQ69G5FAW")
	writeSwapMarker(t, layout.RepoDir(), "old-only", "old")
	update := mustRepoUpdateDir(t, layout, request.Transaction.OperationID)
	writeSwapMarker(t, update, "new-only", "new")
	injected := errors.New("first rename failed")
	operator := &fakeSwapOperator{
		rename: func(context.Context, filesystem.RenameRequest) (filesystem.RenameResult, error) {
			return filesystem.RenameResult{}, injected
		},
		remove: func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
			t.Fatal("RemoveTree() called after first rename failure")
			return filesystem.DeleteResult{}, nil
		},
	}
	transactions := &fakeSwapTransactionWriter{}
	swapper := mustTestSwapper(t, layout, operator, transactions)

	result, err := swapper.Swap(t.Context(), request)
	assertGitrepoCode(t, err, protocol.CodeGitRepoSwapFailed)
	if result.MutationApplied || result.RepositoryActivated {
		t.Fatalf("Swap() result = %#v, want no mutation", result)
	}
	assertSwapMarker(t, filepath.Join(layout.RepoDir(), "old-only"), "old")
	assertSwapMarker(t, filepath.Join(update, "new-only"), "new")
	if _, err := os.Lstat(mustRepoPreviousDir(t, layout, request.Transaction.OperationID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous directory exists after first failure: %v", err)
	}
	if got, want := transactions.stages, []protocol.Stage{protocol.StageWorkspaceSwap}; !equalStages(got, want) {
		t.Fatalf("transaction stages = %v, want %v", got, want)
	}
}

func TestSwapper_SecondRenameFailureLeavesBothCandidates(t *testing.T) {
	layout := mustGitLayout(t)
	request := validSwapRequest(t, layout, "01ARZ3NDEKTSV4RRFFQ69G5FAX")
	writeSwapMarker(t, layout.RepoDir(), "old-only", "old")
	update := mustRepoUpdateDir(t, layout, request.Transaction.OperationID)
	writeSwapMarker(t, update, "new-only", "new")
	injected := errors.New("second rename failed")
	renameCalls := 0
	operator := &fakeSwapOperator{
		rename: func(_ context.Context, request filesystem.RenameRequest) (filesystem.RenameResult, error) {
			renameCalls++
			if renameCalls == 2 {
				return filesystem.RenameResult{}, injected
			}
			if err := os.Rename(request.Source, request.Destination); err != nil {
				return filesystem.RenameResult{}, err
			}
			return filesystem.RenameResult{MutationApplied: true}, nil
		},
		remove: func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
			t.Fatal("RemoveTree() called after second rename failure")
			return filesystem.DeleteResult{}, nil
		},
	}
	swapper := mustTestSwapper(t, layout, operator, &fakeSwapTransactionWriter{})

	result, err := swapper.Swap(t.Context(), request)
	assertGitrepoCode(t, err, protocol.CodeGitRepoSwapFailed)
	if !result.MutationApplied || result.RepositoryActivated {
		t.Fatalf("Swap() result = %#v, want retired but not active", result)
	}
	if _, err := os.Lstat(layout.RepoDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repo exists between renames: %v", err)
	}
	assertSwapMarker(t, filepath.Join(mustRepoPreviousDir(t, layout, request.Transaction.OperationID), "old-only"), "old")
	assertSwapMarker(t, filepath.Join(update, "new-only"), "new")
}

func TestSwapper_RenamePostMutationErrorPreservesFacts(t *testing.T) {
	t.Run("first rename applied", func(t *testing.T) {
		layout := mustGitLayout(t)
		request := validSwapRequest(t, layout, "01ARZ3NDEKTSV4RRFFQ69G5FA4")
		writeSwapMarker(t, layout.RepoDir(), "old-only", "old")
		update := mustRepoUpdateDir(t, layout, request.Transaction.OperationID)
		writeSwapMarker(t, update, "new-only", "new")
		operator := successfulSwapOperator(t)
		operator.rename = func(_ context.Context, rename filesystem.RenameRequest) (filesystem.RenameResult, error) {
			if err := os.Rename(rename.Source, rename.Destination); err != nil {
				return filesystem.RenameResult{}, err
			}
			return filesystem.RenameResult{MutationApplied: true}, errors.New("close after first rename failed")
		}
		operator.remove = func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
			t.Fatal("RemoveTree() called after first rename close failure")
			return filesystem.DeleteResult{}, nil
		}
		swapper := mustTestSwapper(t, layout, operator, &fakeSwapTransactionWriter{})

		result, err := swapper.Swap(t.Context(), request)
		assertGitrepoCode(t, err, protocol.CodeGitRepoSwapFailed)
		if !result.MutationApplied || result.RepositoryActivated {
			t.Fatalf("Swap() result = %#v, want retired mutation fact", result)
		}
		assertSwapMarker(t, filepath.Join(mustRepoPreviousDir(t, layout, request.Transaction.OperationID), "old-only"), "old")
		assertSwapMarker(t, filepath.Join(update, "new-only"), "new")
	})

	t.Run("second rename applied", func(t *testing.T) {
		layout := mustGitLayout(t)
		request := validSwapRequest(t, layout, "01ARZ3NDEKTSV4RRFFQ69G5FA5")
		writeSwapMarker(t, layout.RepoDir(), "old-only", "old")
		writeSwapMarker(t, mustRepoUpdateDir(t, layout, request.Transaction.OperationID), "new-only", "new")
		operator := successfulSwapOperator(t)
		renameCalls := 0
		operator.rename = func(_ context.Context, rename filesystem.RenameRequest) (filesystem.RenameResult, error) {
			renameCalls++
			if err := os.Rename(rename.Source, rename.Destination); err != nil {
				return filesystem.RenameResult{}, err
			}
			if renameCalls == 2 {
				return filesystem.RenameResult{MutationApplied: true}, errors.New("close after second rename failed")
			}
			return filesystem.RenameResult{MutationApplied: true}, nil
		}
		operator.remove = func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
			t.Fatal("RemoveTree() called after second rename close failure")
			return filesystem.DeleteResult{}, nil
		}
		swapper := mustTestSwapper(t, layout, operator, &fakeSwapTransactionWriter{})

		result, err := swapper.Swap(t.Context(), request)
		assertGitrepoCode(t, err, protocol.CodeGitRepoSwapFailed)
		if !result.MutationApplied || !result.RepositoryActivated {
			t.Fatalf("Swap() result = %#v, want active mutation fact", result)
		}
		assertSwapMarker(t, filepath.Join(layout.RepoDir(), "new-only"), "new")
		assertSwapMarker(t, filepath.Join(mustRepoPreviousDir(t, layout, request.Transaction.OperationID), "old-only"), "old")
	})
}

func TestSwapper_MapsOccupiedAndCleanupFailures(t *testing.T) {
	t.Run("occupied", func(t *testing.T) {
		layout := mustGitLayout(t)
		request := validSwapRequest(t, layout, "01ARZ3NDEKTSV4RRFFQ69G5FAY")
		writeSwapMarker(t, layout.RepoDir(), "old-only", "old")
		writeSwapMarker(t, mustRepoUpdateDir(t, layout, request.Transaction.OperationID), "new-only", "new")
		operator := &fakeSwapOperator{
			rename: func(context.Context, filesystem.RenameRequest) (filesystem.RenameResult, error) {
				return filesystem.RenameResult{}, testSwapCodeError{code: protocol.CodeDirectoryOccupied}
			},
			remove: func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
				t.Fatal("RemoveTree() called after occupied rename")
				return filesystem.DeleteResult{}, nil
			},
		}
		swapper := mustTestSwapper(t, layout, operator, &fakeSwapTransactionWriter{})

		result, err := swapper.Swap(t.Context(), request)
		assertGitrepoCode(t, err, protocol.CodeDirectoryOccupied)
		if result.MutationApplied {
			t.Fatalf("Swap() result = %#v, want no mutation", result)
		}
	})

	t.Run("cleanup", func(t *testing.T) {
		layout := mustGitLayout(t)
		request := validSwapRequest(t, layout, "01ARZ3NDEKTSV4RRFFQ69G5FAZ")
		writeSwapMarker(t, layout.RepoDir(), "old-only", "old")
		writeSwapMarker(t, mustRepoUpdateDir(t, layout, request.Transaction.OperationID), "new-only", "new")
		operator := successfulSwapOperator(t)
		operator.remove = func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
			return filesystem.DeleteResult{}, errors.New("retired cleanup failed")
		}
		swapper := mustTestSwapper(t, layout, operator, &fakeSwapTransactionWriter{})

		result, err := swapper.Swap(t.Context(), request)
		assertGitrepoCode(t, err, protocol.CodeGitRepoCleanupFailed)
		if !result.MutationApplied || !result.RepositoryActivated || result.CleanupCompleted {
			t.Fatalf("Swap() result = %#v, want active with retained previous", result)
		}
		assertSwapMarker(t, filepath.Join(layout.RepoDir(), "new-only"), "new")
		assertSwapMarker(t, filepath.Join(mustRepoPreviousDir(t, layout, request.Transaction.OperationID), "old-only"), "old")
	})
}

func TestSwapper_StateWriteFailuresPreserveCommitFacts(t *testing.T) {
	t.Run("before swap", func(t *testing.T) {
		layout := mustGitLayout(t)
		request := validSwapRequest(t, layout, "01ARZ3NDEKTSV4RRFFQ69G5FA0")
		writeSwapMarker(t, layout.RepoDir(), "old-only", "old")
		writeSwapMarker(t, mustRepoUpdateDir(t, layout, request.Transaction.OperationID), "new-only", "new")
		operator := successfulSwapOperator(t)
		operator.rename = func(context.Context, filesystem.RenameRequest) (filesystem.RenameResult, error) {
			t.Fatal("AtomicRename() called after swap transaction write failure")
			return filesystem.RenameResult{}, nil
		}
		transactions := &fakeSwapTransactionWriter{writeErr: func(stage protocol.Stage) error {
			if stage == protocol.StageWorkspaceSwap {
				return errors.New("state write failed")
			}
			return nil
		}}
		swapper := mustTestSwapper(t, layout, operator, transactions)

		result, err := swapper.Swap(t.Context(), request)
		assertGitrepoCode(t, err, protocol.CodeStateWriteFailed)
		if result.MutationApplied {
			t.Fatalf("Swap() result = %#v, want no mutation", result)
		}
	})

	t.Run("after activation", func(t *testing.T) {
		layout := mustGitLayout(t)
		request := validSwapRequest(t, layout, "01ARZ3NDEKTSV4RRFFQ69G5FA1")
		writeSwapMarker(t, layout.RepoDir(), "old-only", "old")
		writeSwapMarker(t, mustRepoUpdateDir(t, layout, request.Transaction.OperationID), "new-only", "new")
		operator := successfulSwapOperator(t)
		operator.remove = func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
			t.Fatal("RemoveTree() called after cleanup transaction write failure")
			return filesystem.DeleteResult{}, nil
		}
		transactions := &fakeSwapTransactionWriter{writeErr: func(stage protocol.Stage) error {
			if stage == protocol.StageWorkspaceCleanup {
				return errors.New("state write failed")
			}
			return nil
		}}
		swapper := mustTestSwapper(t, layout, operator, transactions)

		result, err := swapper.Swap(t.Context(), request)
		assertGitrepoCode(t, err, protocol.CodeStateWriteFailed)
		if !result.MutationApplied || !result.RepositoryActivated || result.CleanupCompleted {
			t.Fatalf("Swap() result = %#v, want active with recovery required", result)
		}
		assertSwapMarker(t, filepath.Join(layout.RepoDir(), "new-only"), "new")
		assertSwapMarker(t, filepath.Join(mustRepoPreviousDir(t, layout, request.Transaction.OperationID), "old-only"), "old")
	})
}

type fakeSwapOperator struct {
	rename func(ctx context.Context, request filesystem.RenameRequest) (filesystem.RenameResult, error)
	remove func(ctx context.Context, request filesystem.DeleteRequest) (filesystem.DeleteResult, error)
}

func (o *fakeSwapOperator) AtomicRename(ctx context.Context, request filesystem.RenameRequest) (filesystem.RenameResult, error) {
	return o.rename(ctx, request)
}

func (o *fakeSwapOperator) RemoveTree(ctx context.Context, request filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
	return o.remove(ctx, request)
}

type fakeSwapTransactionWriter struct {
	stages   []protocol.Stage
	writeErr func(stage protocol.Stage) error
}

func (w *fakeSwapTransactionWriter) WriteTransaction(
	_ context.Context,
	kind state.TransactionKind,
	value state.TransactionState,
) error {
	if kind != state.TransactionUpdate {
		return errors.New("unexpected transaction kind")
	}
	w.stages = append(w.stages, value.Stage)
	if w.writeErr != nil {
		return w.writeErr(value.Stage)
	}
	return nil
}

type testSwapCodeError struct {
	code protocol.Code
}

func (e testSwapCodeError) Error() string       { return "test swap code error" }
func (e testSwapCodeError) Code() protocol.Code { return e.code }

func successfulSwapOperator(t *testing.T) *fakeSwapOperator {
	t.Helper()
	return &fakeSwapOperator{
		rename: func(ctx context.Context, request filesystem.RenameRequest) (filesystem.RenameResult, error) {
			if err := ctx.Err(); err != nil {
				return filesystem.RenameResult{}, err
			}
			if err := os.Rename(request.Source, request.Destination); err != nil {
				return filesystem.RenameResult{}, err
			}
			return filesystem.RenameResult{MutationApplied: true}, nil
		},
		remove: func(ctx context.Context, request filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
			if err := ctx.Err(); err != nil {
				return filesystem.DeleteResult{}, err
			}
			if request.Kind != filesystem.DeleteRepositoryRetired {
				return filesystem.DeleteResult{}, errors.New("unexpected delete kind")
			}
			if err := os.RemoveAll(request.Target); err != nil {
				return filesystem.DeleteResult{}, err
			}
			return filesystem.DeleteResult{Removed: true, AuditCompleted: true}, nil
		},
	}
}

func validSwapRequest(t *testing.T, layout *config.Layout, operationID string) SwapRequest {
	t.Helper()
	target := mustParseTarget(t, "v5.4.0")
	source := mustGitPlan(t, "cnb").Sources()[0]
	revision, err := newRevision(target, testGitCommit, source)
	if err != nil {
		t.Fatalf("newRevision() error = %v", err)
	}
	return SwapRequest{
		Transaction: state.TransactionState{
			SchemaVersion: state.SchemaVersion,
			OperationID:   operationID,
			Command:       "workspace sync",
			PID:           1234,
			StartedAt:     time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC),
			TargetVersion: target.Version(),
			Stage:         protocol.StageWorkspaceVerify,
		},
		Revision: revision,
	}
}

func mustTestSwapper(
	t *testing.T,
	layout *config.Layout,
	operator repositorySwapOperator,
	transactions updateTransactionWriter,
) *Swapper {
	t.Helper()
	swapper, err := newSwapperWithDependencies(layout, operator, transactions)
	if err != nil {
		t.Fatalf("newSwapperWithDependencies() error = %v", err)
	}
	return swapper
}

func mustRepoUpdateDir(t *testing.T, layout *config.Layout, operationID string) string {
	t.Helper()
	path, err := layout.RepoUpdateDir(operationID)
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}
	return path
}

func mustRepoPreviousDir(t *testing.T, layout *config.Layout, operationID string) string {
	t.Helper()
	path, err := layout.RepoPreviousDir(operationID)
	if err != nil {
		t.Fatalf("RepoPreviousDir() error = %v", err)
	}
	return path
}

func writeSwapMarker(t *testing.T, directory, name, value string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("os.MkdirAll(%q) error = %v", directory, err)
	}
	if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", name, err)
	}
}

func assertSwapMarker(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("marker %q = %q, %v, want %q", path, got, err, want)
	}
}

func equalStages(got, want []protocol.Stage) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
