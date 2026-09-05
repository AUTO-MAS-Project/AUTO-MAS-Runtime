package gitrepo

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/lock"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

type stagedTestRuntime struct {
	syncRuntime
	fetch func(context.Context, FetchRequest) (FetchResult, error)
}

func (r stagedTestRuntime) Fetch(ctx context.Context, req FetchRequest) (FetchResult, error) {
	return r.fetch(ctx, req)
}

func stagingFixture(t *testing.T, sameCommit ...bool) (*Service, *config.Layout, SyncRequest, *int) {
	t.Helper()
	layout := componentLayout(t)
	writeRecoveryRepository(t, layout.RepoDir(), "v1.0.0", recoverySourceURL(t), "old")
	service, err := NewService(layout)
	if err != nil {
		t.Fatal(err)
	}
	service.resolveRemote = func(context.Context, mirror.Plan, Target) (string, error) {
		return "", errors.New("network unavailable in staging fixture")
	}
	calls := new(int)
	service.newRuntime = func(ctx context.Context, layout *config.Layout, req SyncRequest, logger OperationLogger) (syncRuntime, error) {
		r, err := newProductionRuntime(ctx, layout, req, logger)
		if err != nil {
			return nil, err
		}
		return stagedTestRuntime{syncRuntime: r, fetch: func(ctx context.Context, fetch FetchRequest) (FetchResult, error) {
			*calls++
			path, err := layout.RepoUpdateDir(fetch.OperationID)
			if err != nil {
				return FetchResult{}, err
			}
			if len(sameCommit) > 0 && sameCommit[0] {
				writeRecoveryRepository(t, path, fetch.Target.Version(), recoverySourceURL(t), "new")
			} else {
				writeRecoveryRepositoryWithExtraCommit(t, path, fetch.Target.Version(), recoverySourceURL(t), "new")
			}
			return service.readPreparedFetch(ctx, state.TransactionState{OperationID: fetch.OperationID}, fetch.Target)
		}}, nil
	}
	current, err := service.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.NewStore(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := st.NewReadyEnvironment(current.Version, current.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteEnvironment(t.Context(), ready); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	policy, err := mirror.NewPolicy(mirror.PolicySpec{})
	if err != nil {
		t.Fatal(err)
	}
	req := componentServiceSyncRequest(layout, componentTarget(t, "v1.0.0"), policy, componentOperationID(70), &recordingServiceEmitter{})
	return service, layout, req, calls
}

func TestService_StageAndActivateOffline(t *testing.T) {
	s, layout, req, calls := stagingFixture(t)
	old, err := os.ReadFile(layout.EnvironmentStateFile())
	if err != nil {
		t.Fatal(err)
	}
	backend, err := lock.NewSet(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	lease, err := backend.AcquireBackend(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := s.Stage(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Staged || prepared.Revision.Commit() == current.Commit {
		t.Fatalf("Stage = %+v, want new commit", prepared)
	}
	again, err := s.Stage(t.Context(), req)
	if err != nil || again.Revision != prepared.Revision || *calls != 1 {
		t.Fatalf("repeated Stage = %+v, %v, fetches=%d", again, err, *calls)
	}
	unchanged, err := s.Check(t.Context())
	if err != nil || unchanged.Commit != current.Commit {
		t.Fatalf("active = %+v, %v, want old commit", unchanged, err)
	}
	data, err := os.ReadFile(layout.EnvironmentStateFile())
	if err != nil || string(data) != string(old) {
		t.Fatalf("environment changed during staging: %v", err)
	}
	if err := lease.Lease().Close(); err != nil {
		t.Fatal(err)
	}
	req.OperationID = componentOperationID(71)
	result, err := s.Sync(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Revision != prepared.Revision || *calls != 1 {
		t.Fatalf("Sync = %+v, fetches=%d, want prepared commit without fetch", result, *calls)
	}
	assertNoComponentTemporaryDirectories(t, layout)
}

func TestService_StageConflictDoesNotWriteTransactions(t *testing.T) {
	s, layout, req, calls := stagingFixture(t)
	set, err := lock.NewSet(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := set.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := set.AcquireMutation(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err = s.Stage(t.Context(), req)
	var coded interface{ Code() protocol.Code }
	if !errors.As(err, &coded) || coded.Code() != protocol.CodeMutationInProgress {
		t.Fatalf("Stage conflict = %v", err)
	}
	if *calls != 0 {
		t.Fatalf("fetches = %d, want 0", *calls)
	}
	if _, err := os.Stat(layout.UpdateStateFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("update transaction = %v, want absent", err)
	}
}

func TestService_StagedUpdateDoesNotBlockVersionUpgrade(t *testing.T) {
	s, layout, req, calls := stagingFixture(t)
	if _, err := s.Stage(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	req.Target = componentTarget(t, "v2.0.0")
	req.OperationID = componentOperationID(72)
	result, err := s.Sync(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision.Version() != "v2.0.0" || *calls != 2 {
		t.Fatalf("Sync = %+v, fetches=%d", result, *calls)
	}
	assertNoComponentTemporaryDirectories(t, layout)
}

func TestService_SyncSameFetchedCommitKeepsEnvironment(t *testing.T) {
	s, layout, req, calls := stagingFixture(t, true)
	old, err := os.ReadFile(layout.EnvironmentStateFile())
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Sync(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || *calls != 1 {
		t.Fatalf("Sync = %+v, fetches=%d, want unchanged after fetch", result, *calls)
	}
	data, err := os.ReadFile(layout.EnvironmentStateFile())
	if err != nil || string(data) != string(old) {
		t.Fatalf("environment changed: %v", err)
	}
	assertNoComponentTemporaryDirectories(t, layout)
}

func TestService_StageSameCommitDoesNotPersistPending(t *testing.T) {
	s, layout, req, _ := stagingFixture(t, true)
	result, err := s.Stage(t.Context(), req)
	if err != nil || result.Staged {
		t.Fatalf("Stage = %+v, %v, want no update", result, err)
	}
	assertNoComponentTemporaryDirectories(t, layout)
}

func TestService_StageInterruptedCloneRecovers(t *testing.T) {
	s, layout, req, calls := stagingFixture(t)
	current, err := s.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.NewStore(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.NewTransaction(state.TransactionUpdate, state.TransactionInput{
		OperationID: req.OperationID, Command: "workspace stage", PID: req.PID,
		TargetVersion: current.Version, BaseCommit: current.Commit, Stage: protocol.StageWorkspaceClone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteTransaction(t.Context(), state.TransactionUpdate, tx); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	path, err := layout.RepoUpdateDir(req.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	req.OperationID = componentOperationID(73)
	result, err := s.Stage(t.Context(), req)
	if err != nil || !result.Staged || *calls != 1 {
		t.Fatalf("Stage after interruption = %+v, %v", result, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted directory = %v, want removed", err)
	}
}

func TestService_StagedCommitMismatchFailsClosed(t *testing.T) {
	s, layout, req, calls := stagingFixture(t)
	if _, err := s.Stage(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	st, err := state.NewStore(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := st.ReadTransaction(t.Context(), state.TransactionUpdate)
	if err != nil {
		t.Fatal(err)
	}
	tx := snapshot.State()
	tx.TargetCommit = testGitCommit
	if err := st.WriteTransaction(t.Context(), state.TransactionUpdate, tx); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := s.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	req.OperationID = componentOperationID(74)
	_, err = s.Sync(t.Context(), req)
	var coded interface{ Code() protocol.Code }
	if !errors.As(err, &coded) || coded.Code() != protocol.CodeUpdateStateAmbiguous {
		t.Fatalf("Sync = %v, want ambiguous", err)
	}
	after, err := s.Check(t.Context())
	if err != nil || before.Commit != after.Commit || *calls != 1 {
		t.Fatalf("active changed: %+v, %v", after, err)
	}
}

func TestRecovery_StagedSameVersionSwapWindows(t *testing.T) {
	for _, phase := range []string{"before-rename", "between-renames", "after-activation", "wrong-base"} {
		t.Run(phase, func(t *testing.T) {
			s, layout, req, _ := stagingFixture(t)
			old, err := s.Check(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := s.Stage(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			st, err := state.NewStore(t.Context(), layout)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := st.ReadTransaction(t.Context(), state.TransactionUpdate)
			if err != nil {
				t.Fatal(err)
			}
			tx := snapshot.State()
			tx.Command = "workspace sync"
			tx.Stage = protocol.StageWorkspaceSwap
			if phase == "wrong-base" {
				tx.BaseCommit = testGitCommit
			}
			if err := st.WriteTransaction(t.Context(), state.TransactionUpdate, tx); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			update, err := layout.RepoUpdateDir(req.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			previous, err := layout.RepoPreviousDir(req.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if phase != "before-rename" {
				if err := os.Rename(layout.RepoDir(), previous); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "after-activation" {
				if err := os.Rename(update, layout.RepoDir()); err != nil {
					t.Fatal(err)
				}
			}
			logger, err := req.LoggerFactory(t.Context(), "workspace-sync", req.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := logger.Close(); err != nil {
					t.Error(err)
				}
			})
			r, err := newProductionRuntime(t.Context(), layout, req, logger)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := r.Close(); err != nil {
					t.Error(err)
				}
			})
			_, err = r.Recover(t.Context(), RecoveryRequest{LogPath: logger.LogPath()})
			if phase == "wrong-base" {
				var coded interface{ Code() protocol.Code }
				if !errors.As(err, &coded) || coded.Code() != protocol.CodeUpdateStateAmbiguous {
					t.Fatalf("Recover = %v, want ambiguous", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			active, err := s.Check(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			want := old.Commit
			if phase == "after-activation" {
				want = prepared.Revision.Commit()
			}
			if active.Commit != want {
				t.Fatalf("active = %s, want %s", active.Commit, want)
			}
			assertNoComponentTemporaryDirectories(t, layout)
		})
	}
}

type cancelAfterRecoveryRuntime struct {
	syncRuntime
	cancel context.CancelFunc
}

func (r cancelAfterRecoveryRuntime) Recover(ctx context.Context, req RecoveryRequest) (RecoveryResult, error) {
	result, err := r.syncRuntime.Recover(ctx, req)
	r.cancel()
	return result, err
}

func TestService_CancelAfterRecoveryPreservesStagedOwnership(t *testing.T) {
	s, layout, req, _ := stagingFixture(t)
	if _, err := s.Stage(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	newRuntime := s.newRuntime
	s.newRuntime = func(ctx context.Context, l *config.Layout, req SyncRequest, logger OperationLogger) (syncRuntime, error) {
		r, err := newRuntime(ctx, l, req, logger)
		return cancelAfterRecoveryRuntime{syncRuntime: r, cancel: cancel}, err
	}
	req.OperationID = componentOperationID(75)
	if _, err := s.Sync(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sync = %v, want cancellation", err)
	}
	if _, err := os.Stat(layout.UpdateStateFile()); err != nil {
		t.Fatalf("pending ownership lost: %v", err)
	}
}
