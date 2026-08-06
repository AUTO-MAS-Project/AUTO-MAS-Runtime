package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

func TestRecovery_CloneInterrupted(t *testing.T) {
	layout := mustGitLayout(t)
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FB0", protocol.StageWorkspaceClone, "v5.4.0")
	update := mustRepoUpdateDir(t, layout, tx.state.OperationID)
	writeSwapMarker(t, update, "partial", "clone")
	if err := os.MkdirAll(filepath.Join(update, ".git", "objects"), 0o700); err != nil {
		t.Fatalf("MkdirAll(partial git) error = %v", err)
	}
	writeRecoveryRepository(t, layout.RepoDir(), "v5.3.0", recoverySourceURL(t), "old")
	operator := successfulRecoveryOperator(t)
	store := &fakeRecoveryStore{transaction: &tx}
	recovery := mustTestRecovery(t, layout, operator, store)
	result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if !result.Recovered || !result.MutationApplied || !result.TransactionRemoved {
		t.Fatalf("Recover() result = %#v, want cleaned transaction", result)
	}
	if _, err := os.Lstat(update); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("update directory remains: %v", err)
	}
	if store.environmentWrites != 0 {
		t.Fatalf("environment writes = %d, want 0", store.environmentWrites)
	}
}

func containsStageSequence(stages []protocol.Stage, want ...protocol.Stage) bool {
	index := 0
	for _, stage := range stages {
		if index < len(want) && stage == want[index] {
			index++
		}
	}
	return index == len(want)
}

func TestRecovery_VerifiedBeforeSwap(t *testing.T) {
	layout := mustGitLayout(t)
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FB1", protocol.StageWorkspaceVerify, "v5.4.0")
	update := mustRepoUpdateDir(t, layout, tx.state.OperationID)
	writeRecoveryRepository(t, update, "v5.4.0", recoverySourceURL(t), "target")
	writeRecoveryRepository(t, layout.RepoDir(), "v5.3.0", recoverySourceURL(t), "old")
	operator := successfulRecoveryOperator(t)
	store := &fakeRecoveryStore{transaction: &tx}
	recovery := mustTestRecovery(t, layout, operator, store)

	result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if !result.Recovered || !result.MutationApplied || !result.TransactionRemoved {
		t.Fatalf("Recover() result = %#v, want update cleanup", result)
	}
	assertSwapMarker(t, filepath.Join(layout.RepoDir(), "marker-old"), "old")
	if _, err := os.Lstat(update); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verified update remains: %v", err)
	}
}

func TestRecovery_BetweenRenamesRollsBack(t *testing.T) {
	layout := mustGitLayout(t)
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FB2", protocol.StageWorkspaceSwap, "v5.4.0")
	previous := mustRepoPreviousDir(t, layout, tx.state.OperationID)
	update := mustRepoUpdateDir(t, layout, tx.state.OperationID)
	writeRecoveryRepository(t, previous, "v5.3.0", recoverySourceURL(t), "old")
	writeRecoveryRepository(t, update, "v5.4.0", recoverySourceURL(t), "target")
	operator := successfulRecoveryOperator(t)
	store := &fakeRecoveryStore{transaction: &tx}
	recovery := mustTestRecovery(t, layout, operator, store)
	var stages []protocol.Stage

	result, err := recovery.Recover(t.Context(), RecoveryRequest{
		LogPath: recoveryLogPath(t, layout),
		StageReporter: func(stage protocol.Stage) {
			stages = append(stages, stage)
		},
	})
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if !result.Recovered || !result.MutationApplied || !result.TransactionRemoved {
		t.Fatalf("Recover() result = %#v, want rollback", result)
	}
	assertSwapMarker(t, filepath.Join(layout.RepoDir(), "marker-old"), "old")
	if _, err := os.Lstat(previous); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous directory remains: %v", err)
	}
	if _, err := os.Lstat(update); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("update directory remains: %v", err)
	}
	if store.environmentWrites != 0 {
		t.Fatalf("environment writes = %d, want 0", store.environmentWrites)
	}
	if !containsStageSequence(stages, protocol.StageWorkspaceSwap, protocol.StageWorkspaceCleanup) {
		t.Fatalf("reported stages = %v, want swap before cleanup", stages)
	}
}

func TestRecovery_SwapBeforeFirstRenameDiscardsCandidate(t *testing.T) {
	layout := mustGitLayout(t)
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FB6", protocol.StageWorkspaceSwap, "v5.4.0")
	update := mustRepoUpdateDir(t, layout, tx.state.OperationID)
	writeRecoveryRepository(t, layout.RepoDir(), "v5.3.0", recoverySourceURL(t), "old")
	writeRecoveryRepository(t, update, "v5.4.0", recoverySourceURL(t), "target")
	store := &fakeRecoveryStore{transaction: &tx}
	recovery := mustTestRecovery(t, layout, successfulRecoveryOperator(t), store)

	result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if !result.Recovered || !result.MutationApplied || !result.TransactionRemoved || result.EnvironmentWritten {
		t.Fatalf("Recover() result = %#v, want discarded candidate", result)
	}
	assertSwapMarker(t, filepath.Join(layout.RepoDir(), "marker-old"), "old")
	if _, err := os.Lstat(update); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("update directory remains: %v", err)
	}
}

func TestRecovery_FirstInstallationPromotesTarget(t *testing.T) {
	layout := mustGitLayout(t)
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FB7", protocol.StageWorkspaceSwap, "v5.4.0")
	update := mustRepoUpdateDir(t, layout, tx.state.OperationID)
	writeRecoveryRepository(t, update, "v5.4.0", recoverySourceURL(t), "target")
	store := &fakeRecoveryStore{transaction: &tx}
	recovery := mustTestRecovery(t, layout, successfulRecoveryOperator(t), store)

	result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if !result.Recovered || !result.MutationApplied || !result.EnvironmentWritten || !result.TransactionRemoved ||
		result.Version != "v5.4.0" || result.SourceKey != "cnb" {
		t.Fatalf("Recover() result = %#v, want promoted target", result)
	}
	assertSwapMarker(t, filepath.Join(layout.RepoDir(), "marker-target"), "target")
	if _, err := os.Lstat(update); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("update directory remains after promotion: %v", err)
	}
}

func TestRecovery_UniquePreviousRestoresWithoutInvalidation(t *testing.T) {
	layout := mustGitLayout(t)
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FB8", protocol.StageWorkspaceSwap, "v5.4.0")
	previous := mustRepoPreviousDir(t, layout, tx.state.OperationID)
	writeRecoveryRepository(t, previous, "v5.3.0", recoverySourceURL(t), "old")
	store := &fakeRecoveryStore{transaction: &tx}
	recovery := mustTestRecovery(t, layout, successfulRecoveryOperator(t), store)

	result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if !result.Recovered || !result.MutationApplied || !result.TransactionRemoved || result.EnvironmentWritten {
		t.Fatalf("Recover() result = %#v, want previous rollback", result)
	}
	assertSwapMarker(t, filepath.Join(layout.RepoDir(), "marker-old"), "old")
}

func TestRecovery_ActiveTargetCompletesEnvironmentState(t *testing.T) {
	layout := mustGitLayout(t)
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FB3", protocol.StageWorkspaceCleanup, "v5.4.0")
	previous := mustRepoPreviousDir(t, layout, tx.state.OperationID)
	writeRecoveryRepository(t, layout.RepoDir(), "v5.4.0", recoverySourceURL(t), "target")
	writeRecoveryRepository(t, previous, "v5.3.0", recoverySourceURL(t), "old")
	store := &fakeRecoveryStore{
		transaction: &tx,
		environment: state.EnvironmentState{
			SchemaVersion: state.SchemaVersion,
			Status:        protocol.StateReadyToStart,
			UpdatedAt:     time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC),
			LastSuccessful: state.Revision{
				Version: "v5.3.0",
				Commit:  testGitCommit,
			},
		},
		environmentSet: true,
	}
	recovery := mustTestRecovery(t, layout, successfulRecoveryOperator(t), store)

	result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if !result.Recovered || !result.MutationApplied || !result.EnvironmentWritten || !result.TransactionRemoved {
		t.Fatalf("Recover() result = %#v, want environment completion", result)
	}
	if store.environment.Status != protocol.StateEnvironmentBroken ||
		store.environment.Broken == nil ||
		store.environment.Broken.Reason != state.ReasonRepositoryChanged ||
		store.environment.Broken.TargetVersion != "v5.4.0" ||
		store.environment.Broken.Commit == "" ||
		store.environment.Broken.Stage != protocol.StageWorkspaceSwap ||
		store.environment.Broken.ExitCode != 0 ||
		store.environment.Broken.PythonVersion != "" ||
		store.environment.Broken.UVVersion != "" {
		t.Fatalf("environment = %#v, want repository_changed broken state", store.environment)
	}
	if store.environment.LastSuccessful != (state.Revision{Version: "v5.3.0", Commit: testGitCommit}) {
		t.Fatalf("lastSuccessful = %#v, want preserved old revision", store.environment.LastSuccessful)
	}
	if _, err := os.Lstat(previous); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous directory remains: %v", err)
	}
}

func TestRecovery_ExistingRepositoryChangedIsNotRewritten(t *testing.T) {
	layout := mustGitLayout(t)
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FB9", protocol.StageWorkspaceCleanup, "v5.4.0")
	previous := mustRepoPreviousDir(t, layout, tx.state.OperationID)
	writeRecoveryRepository(t, layout.RepoDir(), "v5.4.0", recoverySourceURL(t), "target")
	writeRecoveryRepository(t, previous, "v5.3.0", recoverySourceURL(t), "old")
	snapshot, err := (goGitRepositoryReader{}).Inspect(t.Context(), layout.RepoDir())
	if err != nil {
		t.Fatalf("Inspect(active repo) error = %v", err)
	}
	store := &fakeRecoveryStore{
		transaction: &tx,
		environment: state.EnvironmentState{
			SchemaVersion: state.SchemaVersion,
			Status:        protocol.StateEnvironmentBroken,
			UpdatedAt:     time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC),
			Broken: &state.BrokenEnvironment{
				TargetVersion: "v5.4.0",
				Branch:        "release/v5.4.0",
				Commit:        snapshot.commit,
				Reason:        state.ReasonRepositoryChanged,
				Stage:         protocol.StageWorkspaceSwap,
				ExitCode:      0,
				LogPath:       recoveryLogPath(t, layout),
			},
		},
		environmentSet: true,
	}
	recovery := mustTestRecovery(t, layout, successfulRecoveryOperator(t), store)

	result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if !result.Recovered || !result.MutationApplied || result.EnvironmentWritten || !result.TransactionRemoved {
		t.Fatalf("Recover() result = %#v, want idempotent cleanup", result)
	}
	if store.environmentWrites != 0 {
		t.Fatalf("environment writes = %d, want 0", store.environmentWrites)
	}
	if _, err := os.Lstat(previous); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous directory remains: %v", err)
	}
}

func TestRecovery_PreservesBrokenVenvState(t *testing.T) {
	layout := mustGitLayout(t)
	store := &fakeRecoveryStore{
		environment: state.EnvironmentState{
			SchemaVersion: state.SchemaVersion,
			Status:        protocol.StateEnvironmentBroken,
			UpdatedAt:     time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC),
			Broken: &state.BrokenEnvironment{
				TargetVersion: "v5.4.0",
				Branch:        "release/v5.4.0",
				Commit:        testGitCommit,
				PythonVersion: "3.12.4",
				UVVersion:     "0.8.0",
				Reason:        state.ReasonOperationFailed,
				Stage:         protocol.StagePythonInstall,
				ExitCode:      1,
				LogPath:       recoveryLogPath(t, layout),
			},
		},
		environmentSet: true,
	}
	recovery := mustTestRecovery(t, layout, successfulRecoveryOperator(t), store)

	result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if result.Recovered || result.MutationApplied || result.EnvironmentWritten || result.TransactionRemoved {
		t.Fatalf("Recover() result = %#v, want no-op", result)
	}
	if store.environmentWrites != 0 {
		t.Fatalf("environment writes = %d, want 0", store.environmentWrites)
	}
}

func TestRecovery_AmbiguousIdentityHasNoSideEffects(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, layout *config.Layout, tx state.TransactionState)
	}{
		{
			name: "same target different commits",
			setup: func(t *testing.T, layout *config.Layout, tx state.TransactionState) {
				writeRecoveryRepository(t, layout.RepoDir(), "v5.4.0", recoverySourceURL(t), "active")
				update := mustRepoUpdateDir(t, layout, tx.OperationID)
				writeRecoveryRepositoryWithExtraCommit(t, update, "v5.4.0", recoverySourceURL(t), "candidate")
			},
		},
		{
			name: "reparse-like non-directory",
			setup: func(t *testing.T, layout *config.Layout, tx state.TransactionState) {
				if err := os.MkdirAll(layout.AppRoot(), 0o700); err != nil {
					t.Fatalf("MkdirAll(app root) error = %v", err)
				}
				update := mustRepoUpdateDir(t, layout, tx.OperationID)
				if err := os.WriteFile(update, []byte("foreign"), 0o600); err != nil {
					t.Fatalf("WriteFile(update) error = %v", err)
				}
			},
		},
		{
			name: "unknown remote",
			setup: func(t *testing.T, layout *config.Layout, _ state.TransactionState) {
				writeRecoveryRepository(t, layout.RepoDir(), "v5.4.0", "https://example.test/untrusted.git", "untrusted")
			},
		},
		{
			name: "directory symlink",
			setup: func(t *testing.T, layout *config.Layout, tx state.TransactionState) {
				if err := os.MkdirAll(layout.AppRoot(), 0o700); err != nil {
					t.Fatalf("MkdirAll(app root) error = %v", err)
				}
				update := mustRepoUpdateDir(t, layout, tx.OperationID)
				if err := os.Symlink(t.TempDir(), update); err != nil {
					t.Skipf("directory symlink unavailable: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout := mustGitLayout(t)
			tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FB4", protocol.StageWorkspaceSwap, "v5.4.0")
			test.setup(t, layout, tx.state)
			operator := &fakeRecoveryOperator{
				rename: func(context.Context, filesystem.RenameRequest) (filesystem.RenameResult, error) {
					t.Fatal("AtomicRename() called for ambiguous identity")
					return filesystem.RenameResult{}, nil
				},
				remove: func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
					t.Fatal("RemoveTree() called for ambiguous identity")
					return filesystem.DeleteResult{}, nil
				},
			}
			store := &fakeRecoveryStore{transaction: &tx}
			recovery := mustTestRecovery(t, layout, operator, store)

			result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
			assertGitrepoCode(t, err, protocol.CodeUpdateStateAmbiguous)
			if result.MutationApplied || result.EnvironmentWritten || result.TransactionRemoved {
				t.Fatalf("Recover() result = %#v, want zero side effects", result)
			}
		})
	}
}

func TestRecovery_ReparseAncestorHasNoSideEffects(t *testing.T) {
	root := t.TempDir()
	container := filepath.Join(root, "container")
	external := filepath.Join(root, "external")
	if err := os.MkdirAll(container, 0o755); err != nil {
		t.Fatalf("MkdirAll(container) error = %v", err)
	}
	if err := os.MkdirAll(external, 0o755); err != nil {
		t.Fatalf("MkdirAll(external) error = %v", err)
	}
	alias := filepath.Join(container, "alias")
	mustCreateGitRepoJunction(t, alias, external)
	layout, err := config.NewLayout(filepath.Join(alias, "app"), container)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FC0", protocol.StageWorkspaceClone, "v5.4.0")
	update := mustRepoUpdateDir(t, layout, tx.state.OperationID)
	writeRecoveryRepository(t, update, "v5.4.0", recoverySourceURL(t), "target")
	operator := &fakeRecoveryOperator{
		rename: func(context.Context, filesystem.RenameRequest) (filesystem.RenameResult, error) {
			t.Fatal("AtomicRename() called for reparse ancestor")
			return filesystem.RenameResult{}, nil
		},
		remove: func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
			t.Fatal("RemoveTree() called for reparse ancestor")
			return filesystem.DeleteResult{}, nil
		},
	}
	store := &fakeRecoveryStore{transaction: &tx}
	recovery := mustTestRecovery(t, layout, operator, store)
	result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
	assertGitrepoCode(t, err, protocol.CodeUpdateStateAmbiguous)
	if result.MutationApplied || result.EnvironmentWritten || result.TransactionRemoved {
		t.Fatalf("Recover() result = %#v, want zero side effects", result)
	}
	if _, err := os.Stat(update); err != nil {
		t.Fatalf("update directory disappeared after ambiguous reparse: %v", err)
	}
}

func TestRecovery_PreSwapSameTargetDifferentCommitsHasNoSideEffects(t *testing.T) {
	layout := mustGitLayout(t)
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FBD", protocol.StageWorkspaceVerify, "v5.4.0")
	writeRecoveryRepository(t, layout.RepoDir(), "v5.4.0", recoverySourceURL(t), "active")
	update := mustRepoUpdateDir(t, layout, tx.state.OperationID)
	writeRecoveryRepositoryWithExtraCommit(t, update, "v5.4.0", recoverySourceURL(t), "candidate")
	store := &fakeRecoveryStore{transaction: &tx}
	recovery := mustTestRecovery(t, layout, noSideEffectRecoveryOperator(t), store)

	result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
	assertGitrepoCode(t, err, protocol.CodeUpdateStateAmbiguous)
	if result.MutationApplied || result.EnvironmentWritten || result.TransactionRemoved || store.transactionRemoved {
		t.Fatalf("Recover() result = %#v, want zero side effects", result)
	}
	assertSwapMarker(t, filepath.Join(layout.RepoDir(), "marker-active"), "active")
	assertSwapMarker(t, filepath.Join(update, "marker-candidate"), "candidate")
}

func TestRecovery_PreSwapSameTargetCommitFromDifferentSourcesIsEquivalent(t *testing.T) {
	layout := mustGitLayout(t)
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FBE", protocol.StageWorkspaceVerify, "v5.4.0")
	update := mustRepoUpdateDir(t, layout, tx.state.OperationID)
	writeRecoveryRepository(t, layout.RepoDir(), "v5.4.0", recoverySourceURLForKey(t, "cnb"), "active")
	writeRecoveryRepository(t, update, "v5.4.0", recoverySourceURLForKey(t, "github"), "candidate")
	activeSnapshot, err := (goGitRepositoryReader{}).Inspect(t.Context(), layout.RepoDir())
	if err != nil {
		t.Fatalf("Inspect(active) error = %v", err)
	}
	updateSnapshot, err := (goGitRepositoryReader{}).Inspect(t.Context(), update)
	if err != nil {
		t.Fatalf("Inspect(update) error = %v", err)
	}
	if activeSnapshot.commit != updateSnapshot.commit {
		t.Fatalf("fixture commits = %q/%q, want same commit", activeSnapshot.commit, updateSnapshot.commit)
	}
	store := &fakeRecoveryStore{transaction: &tx}
	recovery := mustTestRecovery(t, layout, successfulRecoveryOperator(t), store)

	result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if !result.Recovered || !result.MutationApplied || !result.TransactionRemoved {
		t.Fatalf("Recover() result = %#v, want equivalent candidate cleanup", result)
	}
	assertSwapMarker(t, filepath.Join(layout.RepoDir(), "marker-active"), "active")
	if _, err := os.Lstat(update); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("equivalent update directory remains: %v", err)
	}
}

func TestRecovery_SwapSameTargetCommitFromDifferentSourcesIsNotMultipleRevision(t *testing.T) {
	layout := mustGitLayout(t)
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FBF", protocol.StageWorkspaceSwap, "v5.4.0")
	update := mustRepoUpdateDir(t, layout, tx.state.OperationID)
	writeRecoveryRepository(t, layout.RepoDir(), "v5.4.0", recoverySourceURLForKey(t, "cnb"), "active")
	writeRecoveryRepository(t, update, "v5.4.0", recoverySourceURLForKey(t, "github"), "candidate")
	activeSnapshot, err := (goGitRepositoryReader{}).Inspect(t.Context(), layout.RepoDir())
	if err != nil {
		t.Fatalf("Inspect(active) error = %v", err)
	}
	updateSnapshot, err := (goGitRepositoryReader{}).Inspect(t.Context(), update)
	if err != nil {
		t.Fatalf("Inspect(update) error = %v", err)
	}
	if activeSnapshot.commit != updateSnapshot.commit {
		t.Fatalf("fixture commits = %q/%q, want same commit", activeSnapshot.commit, updateSnapshot.commit)
	}
	store := &fakeRecoveryStore{transaction: &tx}
	recovery := mustTestRecovery(t, layout, successfulRecoveryOperator(t), store)

	result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
	assertGitrepoCode(t, err, protocol.CodeUpdateStateAmbiguous)
	var operationErr *Error
	if !errors.As(err, &operationErr) || operationErr.Details()["reason"] != "active_target_shape" {
		t.Fatalf("Recover() error details = %#v, want active_target_shape", operationErr.Details())
	}
	if result.MutationApplied || result.EnvironmentWritten || result.TransactionRemoved {
		t.Fatalf("Recover() result = %#v, want shape ambiguity without mutation", result)
	}
	assertSwapMarker(t, filepath.Join(layout.RepoDir(), "marker-active"), "active")
	assertSwapMarker(t, filepath.Join(update, "marker-candidate"), "candidate")
}

func TestRecovery_CorruptStateHasNoDirectorySideEffects(t *testing.T) {
	t.Run("update transaction", func(t *testing.T) {
		layout := mustGitLayout(t)
		operator := noSideEffectRecoveryOperator(t)
		store := &fakeRecoveryStore{
			readUpdateErr: &state.CorruptError{File: "update", Cause: errors.New("invalid update")},
		}
		recovery := mustTestRecovery(t, layout, operator, store)

		result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
		assertGitrepoCode(t, err, protocol.CodeUpdateStateAmbiguous)
		if result.MutationApplied || result.TransactionRemoved {
			t.Fatalf("Recover() result = %#v, want zero side effects", result)
		}
	})

	t.Run("environment", func(t *testing.T) {
		layout := mustGitLayout(t)
		tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FBB", protocol.StageWorkspaceCleanup, "v5.4.0")
		previous := mustRepoPreviousDir(t, layout, tx.state.OperationID)
		writeRecoveryRepository(t, layout.RepoDir(), "v5.4.0", recoverySourceURL(t), "target")
		writeRecoveryRepository(t, previous, "v5.3.0", recoverySourceURL(t), "old")
		store := &fakeRecoveryStore{
			transaction:        &tx,
			readEnvironmentErr: &state.CorruptError{File: "environment", Cause: errors.New("invalid environment")},
		}
		recovery := mustTestRecovery(t, layout, noSideEffectRecoveryOperator(t), store)

		result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
		assertGitrepoCode(t, err, protocol.CodeUpdateStateAmbiguous)
		if result.MutationApplied || result.EnvironmentWritten || result.TransactionRemoved {
			t.Fatalf("Recover() result = %#v, want zero side effects", result)
		}
		assertSwapMarker(t, filepath.Join(previous, "marker-old"), "old")
	})
}

func TestRecovery_RepositoryIdentityRejectsEmptyRemoteURL(t *testing.T) {
	_, err := repositoryIdentityFromSnapshot(repositorySnapshot{
		nonBare:      true,
		remotes:      []remoteSnapshot{{name: "origin"}},
		headSymbolic: true,
		headTarget:   "refs/heads/release/v5.4.0",
		commit:       testGitCommit,
		shallow:      []string{testGitCommit},
		versionMode:  filemode.Regular,
		versionPayload: []byte(
			`{"version":"v5.4.0"}`,
		),
	})
	if err == nil {
		t.Fatal("repositoryIdentityFromSnapshot() error = nil, want invalid remote")
	}
}

func TestRecovery_FirstInstallRenameFailureDoesNotClaimMutation(t *testing.T) {
	layout := mustGitLayout(t)
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FBA", protocol.StageWorkspaceSwap, "v5.4.0")
	update := mustRepoUpdateDir(t, layout, tx.state.OperationID)
	writeRecoveryRepository(t, update, "v5.4.0", recoverySourceURL(t), "target")
	operator := successfulRecoveryOperator(t)
	operator.rename = func(context.Context, filesystem.RenameRequest) (filesystem.RenameResult, error) {
		return filesystem.RenameResult{}, errors.New("promotion failed")
	}
	store := &fakeRecoveryStore{transaction: &tx}
	recovery := mustTestRecovery(t, layout, operator, store)

	result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
	assertGitrepoCode(t, err, protocol.CodeGitRepoSwapFailed)
	if result.MutationApplied || result.TransactionRemoved || store.transactionRemoved {
		t.Fatalf("Recover() result = %#v, want retained pre-mutation transaction", result)
	}
	assertSwapMarker(t, filepath.Join(update, "marker-target"), "target")
}

func TestRecovery_StateWriteFailureRetainsTransaction(t *testing.T) {
	layout := mustGitLayout(t)
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FB5", protocol.StageWorkspaceSwap, "v5.4.0")
	writeRecoveryRepository(t, layout.RepoDir(), "v5.4.0", recoverySourceURL(t), "target")
	store := &fakeRecoveryStore{
		transaction: &tx,
		environment: state.EnvironmentState{
			SchemaVersion: state.SchemaVersion,
			Status:        protocol.StateReadyToStart,
			UpdatedAt:     time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC),
			LastSuccessful: state.Revision{
				Version: "v5.3.0",
				Commit:  testGitCommit,
			},
		},
		environmentSet: true,
		writeErr:       errors.New("environment state write failed"),
	}
	recovery := mustTestRecovery(t, layout, successfulRecoveryOperator(t), store)

	result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
	assertGitrepoCode(t, err, protocol.CodeStateWriteFailed)
	if result.TransactionRemoved || !result.MutationApplied {
		t.Fatalf("Recover() result = %#v, want active repo and retained transaction", result)
	}
	if store.transactionRemoved {
		t.Fatal("transaction was removed after state write failure")
	}
}

func TestRecovery_ActiveCleanupDeadlineUsesCommittedErrorCodes(t *testing.T) {
	t.Run("environment write is state failure", func(t *testing.T) {
		layout := mustGitLayout(t)
		tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FC1", protocol.StageWorkspaceCleanup, "v5.4.0")
		writeRecoveryRepository(t, layout.RepoDir(), "v5.4.0", recoverySourceURL(t), "target")
		store := &fakeRecoveryStore{
			transaction: &tx,
			environment: state.EnvironmentState{
				SchemaVersion: state.SchemaVersion,
				Status:        protocol.StateReadyToStart,
			},
			environmentSet: true,
			writeErr:       context.DeadlineExceeded,
		}
		recovery := mustTestRecovery(t, layout, successfulRecoveryOperator(t), store)

		result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
		assertGitrepoCode(t, err, protocol.CodeStateWriteFailed)
		if result.TransactionRemoved || store.transactionRemoved {
			t.Fatalf("Recover() result = %#v, want active transaction retained", result)
		}
	})

	t.Run("retired cleanup is repository cleanup failure", func(t *testing.T) {
		layout := mustGitLayout(t)
		tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FC2", protocol.StageWorkspaceCleanup, "v5.4.0")
		previous := mustRepoPreviousDir(t, layout, tx.state.OperationID)
		writeRecoveryRepository(t, layout.RepoDir(), "v5.4.0", recoverySourceURL(t), "target")
		writeRecoveryRepository(t, previous, "v5.3.0", recoverySourceURL(t), "old")
		store := &fakeRecoveryStore{
			transaction: &tx,
			environment: state.EnvironmentState{
				SchemaVersion: state.SchemaVersion,
				Status:        protocol.StateReadyToStart,
			},
			environmentSet: true,
		}
		operator := successfulRecoveryOperator(t)
		operator.remove = func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
			return filesystem.DeleteResult{}, context.DeadlineExceeded
		}
		recovery := mustTestRecovery(t, layout, operator, store)

		result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
		assertGitrepoCode(t, err, protocol.CodeGitRepoCleanupFailed)
		if result.TransactionRemoved || store.transactionRemoved {
			t.Fatalf("Recover() result = %#v, want active transaction retained", result)
		}
	})

	t.Run("transaction removal is state failure", func(t *testing.T) {
		layout := mustGitLayout(t)
		tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FC3", protocol.StageWorkspaceCleanup, "v5.4.0")
		writeRecoveryRepository(t, layout.RepoDir(), "v5.4.0", recoverySourceURL(t), "target")
		store := &fakeRecoveryStore{
			transaction: &tx,
			environment: state.EnvironmentState{
				SchemaVersion: state.SchemaVersion,
				Status:        protocol.StateReadyToStart,
			},
			environmentSet: true,
			removeErr:      context.DeadlineExceeded,
		}
		recovery := mustTestRecovery(t, layout, successfulRecoveryOperator(t), store)

		result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
		assertGitrepoCode(t, err, protocol.CodeStateWriteFailed)
		if !result.EnvironmentWritten || result.TransactionRemoved || store.transactionRemoved {
			t.Fatalf("Recover() result = %#v, want environment completion with retained transaction", result)
		}
	})
}

func TestRecovery_TransactionRemovalFailureRetainsTransaction(t *testing.T) {
	layout := mustGitLayout(t)
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FBC", protocol.StageWorkspaceClone, "v5.4.0")
	update := mustRepoUpdateDir(t, layout, tx.state.OperationID)
	writeSwapMarker(t, update, "partial", "clone")
	store := &fakeRecoveryStore{
		transaction: &tx,
		removeErr:   errors.New("transaction removal failed"),
	}
	recovery := mustTestRecovery(t, layout, successfulRecoveryOperator(t), store)

	var stages []protocol.Stage
	result, err := recovery.Recover(t.Context(), RecoveryRequest{
		LogPath: recoveryLogPath(t, layout),
		StageReporter: func(stage protocol.Stage) {
			stages = append(stages, stage)
		},
	})
	assertGitrepoCode(t, err, protocol.CodeStateWriteFailed)
	var operationErr *Error
	if !errors.As(err, &operationErr) {
		t.Fatalf("error = %v, want *Error", err)
	}
	if got, want := operationErr.Stage(), protocol.StageWorkspaceCleanup; got != want {
		t.Fatalf("error stage = %q, want %q", got, want)
	}
	if len(stages) == 0 || stages[len(stages)-1] != protocol.StageWorkspaceCleanup {
		t.Fatalf("reported stages = %v, want cleanup as final stage", stages)
	}
	if !result.MutationApplied || result.TransactionRemoved || store.transactionRemoved {
		t.Fatalf("Recover() result = %#v, want cleaned directory and retained transaction", result)
	}
	if _, err := os.Lstat(update); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("update directory remains: %v", err)
	}
}

func TestRecovery_CloneReaderFailureTreatsOrdinaryUpdateAsIncomplete(t *testing.T) {
	layout := mustGitLayout(t)
	tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FBE", protocol.StageWorkspaceClone, "v5.4.0")
	update := mustRepoUpdateDir(t, layout, tx.state.OperationID)
	if err := os.MkdirAll(update, 0o700); err != nil {
		t.Fatalf("MkdirAll(update) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(update, "partial.bin"), []byte("partial"), 0o600); err != nil {
		t.Fatalf("WriteFile(partial) error = %v", err)
	}
	store := &fakeRecoveryStore{transaction: &tx}
	recovery, err := newRecoveryWithDependencies(
		layout,
		successfulRecoveryOperator(t),
		store,
		&fakeRepositoryReader{err: errors.New("repository metadata sentinel")},
	)
	if err != nil {
		t.Fatalf("newRecoveryWithDependencies() error = %v", err)
	}

	result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
	if err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	if !result.Recovered || !result.MutationApplied || !result.TransactionRemoved {
		t.Fatalf("Recover() result = %#v, want incomplete update cleanup", result)
	}
	if _, err := os.Lstat(update); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("update directory remains: %v", err)
	}
}

func TestRecovery_NonCloneReaderFailureRemainsAmbiguous(t *testing.T) {
	tests := []struct {
		name  string
		stage protocol.Stage
	}{
		{name: "verify", stage: protocol.StageWorkspaceVerify},
		{name: "swap", stage: protocol.StageWorkspaceSwap},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout := mustGitLayout(t)
			tx := newRecoveryTransaction(t, "01ARZ3NDEKTSV4RRFFQ69G5FBF", test.stage, "v5.4.0")
			update := mustRepoUpdateDir(t, layout, tx.state.OperationID)
			if err := os.MkdirAll(update, 0o700); err != nil {
				t.Fatalf("MkdirAll(update) error = %v", err)
			}
			if err := os.WriteFile(filepath.Join(update, "partial.bin"), []byte("partial"), 0o600); err != nil {
				t.Fatalf("WriteFile(partial) error = %v", err)
			}
			store := &fakeRecoveryStore{transaction: &tx}
			recovery, err := newRecoveryWithDependencies(
				layout,
				successfulRecoveryOperator(t),
				store,
				&fakeRepositoryReader{err: errors.New("repository metadata sentinel")},
			)
			if err != nil {
				t.Fatalf("newRecoveryWithDependencies() error = %v", err)
			}

			result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
			assertGitrepoCode(t, err, protocol.CodeUpdateStateAmbiguous)
			var operationErr *Error
			if !errors.As(err, &operationErr) {
				t.Fatalf("error = %v, want *Error", err)
			}
			if got, want := operationErr.Stage(), test.stage; got != want {
				t.Fatalf("error stage = %q, want %q", got, want)
			}
			if result.MutationApplied || result.TransactionRemoved || store.transactionRemoved {
				t.Fatalf("Recover() result = %#v, want no side effects", result)
			}
			if _, err := os.Lstat(update); err != nil {
				t.Fatalf("update directory disappeared after ambiguous recovery: %v", err)
			}
		})
	}
}

type fakeRecoveryOperator struct {
	rename func(ctx context.Context, request filesystem.RenameRequest) (filesystem.RenameResult, error)
	remove func(ctx context.Context, request filesystem.DeleteRequest) (filesystem.DeleteResult, error)
}

func (o *fakeRecoveryOperator) AtomicRename(ctx context.Context, request filesystem.RenameRequest) (filesystem.RenameResult, error) {
	return o.rename(ctx, request)
}

func (o *fakeRecoveryOperator) RemoveTree(ctx context.Context, request filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
	return o.remove(ctx, request)
}

type fakeRecoveryStore struct {
	transaction        *recoveryTransaction
	transactionRemoved bool
	readUpdateErr      error
	environment        state.EnvironmentState
	environmentSet     bool
	readEnvironmentErr error
	environmentWrites  int
	writeErr           error
	removeErr          error
}

func (s *fakeRecoveryStore) ReadUpdate(context.Context) (recoveryTransaction, error) {
	if s.readUpdateErr != nil {
		return recoveryTransaction{}, s.readUpdateErr
	}
	if s.transaction == nil {
		return recoveryTransaction{}, &state.NotFoundError{File: "update", Path: "update.json"}
	}
	value := *s.transaction
	value.remove = s.removeTransaction
	return value, nil
}

func (s *fakeRecoveryStore) ReadEnvironment(context.Context) (state.EnvironmentState, error) {
	if s.readEnvironmentErr != nil {
		return state.EnvironmentState{}, s.readEnvironmentErr
	}
	if !s.environmentSet {
		return state.EnvironmentState{}, &state.NotFoundError{File: "environment", Path: "environment.json"}
	}
	return s.environment, nil
}

func (s *fakeRecoveryStore) NewBrokenEnvironment(
	lastSuccessful state.Revision,
	broken state.BrokenEnvironment,
) (state.EnvironmentState, error) {
	return state.EnvironmentState{
		SchemaVersion:  state.SchemaVersion,
		Status:         protocol.StateEnvironmentBroken,
		UpdatedAt:      time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC),
		LastSuccessful: lastSuccessful,
		Broken:         &broken,
	}, nil
}

func (s *fakeRecoveryStore) WriteEnvironment(_ context.Context, value state.EnvironmentState) error {
	s.environmentWrites++
	if s.writeErr != nil {
		return s.writeErr
	}
	s.environment = value
	s.environmentSet = true
	return nil
}

func (s *fakeRecoveryStore) removeTransaction(context.Context) error {
	if s.removeErr != nil {
		return s.removeErr
	}
	s.transactionRemoved = true
	return nil
}

func newRecoveryTransaction(t *testing.T, operationID string, stage protocol.Stage, version string) recoveryTransaction {
	t.Helper()
	return recoveryTransaction{state: state.TransactionState{
		SchemaVersion: state.SchemaVersion,
		OperationID:   operationID,
		Command:       "workspace sync",
		PID:           1234,
		StartedAt:     time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC),
		TargetVersion: version,
		Stage:         stage,
	}}
}

func successfulRecoveryOperator(t *testing.T) *fakeRecoveryOperator {
	t.Helper()
	return &fakeRecoveryOperator{
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
			if err := os.RemoveAll(request.Target); err != nil {
				return filesystem.DeleteResult{}, err
			}
			return filesystem.DeleteResult{Removed: true, AuditCompleted: true}, nil
		},
	}
}

func noSideEffectRecoveryOperator(t *testing.T) *fakeRecoveryOperator {
	t.Helper()
	return &fakeRecoveryOperator{
		rename: func(context.Context, filesystem.RenameRequest) (filesystem.RenameResult, error) {
			t.Fatal("AtomicRename() called before ambiguous state was rejected")
			return filesystem.RenameResult{}, nil
		},
		remove: func(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error) {
			t.Fatal("RemoveTree() called before ambiguous state was rejected")
			return filesystem.DeleteResult{}, nil
		},
	}
}

func mustTestRecovery(
	t *testing.T,
	layout *config.Layout,
	operator repositorySwapOperator,
	store recoveryStateStore,
) *Recovery {
	t.Helper()
	recovery, err := newRecoveryWithDependencies(layout, operator, store, goGitRepositoryReader{})
	if err != nil {
		t.Fatalf("newRecoveryWithDependencies() error = %v", err)
	}
	return recovery
}

func recoveryLogPath(t *testing.T, layout *config.Layout) string {
	t.Helper()
	path, err := layout.RuntimeLogFile("workspace-sync", time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("RuntimeLogFile() error = %v", err)
	}
	return path
}

func recoverySourceURL(t *testing.T) string {
	return recoverySourceURLForKey(t, "cnb")
}

func recoverySourceURLForKey(t *testing.T, key string) string {
	t.Helper()
	return mustGitPlan(t, key).Sources()[0].BaseURL()
}

func writeRecoveryRepository(t *testing.T, destination, version, sourceURL, marker string) {
	t.Helper()
	source, _ := createVerifiedRepository(t, version, sourceURL)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatalf("MkdirAll(destination parent) error = %v", err)
	}
	if err := os.Rename(source, destination); err != nil {
		t.Fatalf("Rename(repository) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(destination, "marker-"+marker), []byte(marker), 0o600); err != nil {
		t.Fatalf("WriteFile(marker) error = %v", err)
	}
}

func writeRecoveryRepositoryWithExtraCommit(t *testing.T, destination, version, sourceURL, marker string) {
	t.Helper()
	writeRecoveryRepository(t, destination, version, sourceURL, marker)
	repository, err := git.PlainOpen(destination)
	if err != nil {
		t.Fatalf("PlainOpen() error = %v", err)
	}
	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatalf("Worktree() error = %v", err)
	}
	extra := filepath.Join(destination, "extra.txt")
	if err := os.WriteFile(extra, []byte("different commit"), 0o600); err != nil {
		t.Fatalf("WriteFile(extra) error = %v", err)
	}
	if _, err := worktree.Add("extra.txt"); err != nil {
		t.Fatalf("Add(extra) error = %v", err)
	}
	hash, err := worktree.Commit("second release commit", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "AUTO-MAS test",
			Email: "test@example.invalid",
			When:  time.Date(2026, 8, 6, 0, 0, 0, 0, time.UTC),
		},
	})
	if err != nil {
		t.Fatalf("Commit(extra) error = %v", err)
	}
	branch := plumbing.NewBranchReferenceName("release/" + version)
	if err := repository.Storer.SetReference(plumbing.NewHashReference(branch, hash)); err != nil {
		t.Fatalf("SetReference(branch) error = %v", err)
	}
	if err := repository.Storer.SetShallow([]plumbing.Hash{hash}); err != nil {
		t.Fatalf("SetShallow() error = %v", err)
	}
}
