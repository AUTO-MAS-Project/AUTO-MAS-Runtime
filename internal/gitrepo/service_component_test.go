package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

// TestService_SyncVersionDowngradeUsesSameFlow 证明旧版本目标仍走完整 Service 编排。
func TestService_SyncVersionDowngradeUsesSameFlow(t *testing.T) {
	repository := newGitFixtureRepository(t,
		gitFixtureCommit{label: "old", version: "v1.0.0"},
		gitFixtureCommit{label: "new", version: "v2.0.0"},
	)
	repository.setBranch(t, "v1.0.0", "old")
	repository.setBranch(t, "v2.0.0", "new")
	server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
	source := server.source(t, "origin", "origin", true)
	plan := gitFixturePlan(t, []mirror.Source{source}, "")
	policy, err := mirror.NewPolicy(mirror.PolicySpec{})
	if err != nil {
		t.Fatalf("mirror.NewPolicy() error = %v", err)
	}
	layout := componentLayout(t)
	seedStore, err := state.NewStore(t.Context(), layout)
	if err != nil {
		t.Fatalf("state.NewStore(seed) error = %v", err)
	}
	ready, err := seedStore.NewReadyEnvironment("v0.9.0", strings.Repeat("a", 40))
	if err != nil {
		_ = seedStore.Close()
		t.Fatalf("NewReadyEnvironment() error = %v", err)
	}
	if err := seedStore.WriteEnvironment(t.Context(), ready); err != nil {
		_ = seedStore.Close()
		t.Fatalf("WriteEnvironment(seed) error = %v", err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatalf("seed Store.Close() error = %v", err)
	}

	service, err := newServiceWithDependencies(
		layout,
		goGitRepositoryReader{},
		func(context.Context, *config.Layout) (mutationLockSet, error) {
			return &serviceTestLocks{}, nil
		},
		func(ctx context.Context, runtimeLayout *config.Layout, request SyncRequest, _ OperationLogger) (syncRuntime, error) {
			store, storeErr := state.NewStore(ctx, runtimeLayout, state.WithClock(request.Clock))
			if storeErr != nil {
				return nil, storeErr
			}
			fetcher, operator := componentFetcher(t, runtimeLayout, server.caBundle)
			recovery, recoveryErr := NewRecovery(runtimeLayout, operator, store)
			if recoveryErr != nil {
				_ = store.Close()
				return nil, recoveryErr
			}
			swapper, swapErr := NewSwapper(runtimeLayout, operator, store)
			if swapErr != nil {
				_ = store.Close()
				return nil, swapErr
			}
			return &productionRuntime{
				store:    store,
				recovery: recovery,
				fetcher:  fetcher,
				swapper:  swapper,
			}, nil
		},
		func(mirror.Policy) (mirror.Plan, error) { return plan, nil },
	)
	if err != nil {
		t.Fatalf("newServiceWithDependencies() error = %v", err)
	}

	newTarget := componentTarget(t, "v2.0.0")
	oldTarget := componentTarget(t, "v1.0.0")
	first := &recordingServiceEmitter{}
	firstResult, err := service.Sync(t.Context(), componentServiceSyncRequest(
		layout,
		newTarget,
		policy,
		componentOperationID(40),
		first,
	))
	if err != nil {
		t.Fatalf("Sync(new) error = %v", err)
	}
	if !firstResult.Changed || firstResult.Status != protocol.StateEnvironmentBroken {
		t.Fatalf("Sync(new) result = %#v, want changed environment_broken", firstResult)
	}

	second := &recordingServiceEmitter{}
	secondResult, err := service.Sync(t.Context(), componentServiceSyncRequest(
		layout,
		oldTarget,
		policy,
		componentOperationID(41),
		second,
	))
	if err != nil {
		t.Fatalf("Sync(old) error = %v", err)
	}
	oldCommit := repository.hash(t, "old").String()
	if !secondResult.Changed || secondResult.Status != protocol.StateEnvironmentBroken ||
		secondResult.Revision.Version() != oldTarget.Version() ||
		secondResult.Revision.Branch() != oldTarget.Branch() ||
		secondResult.Revision.Commit() != oldCommit {
		t.Fatalf("Sync(old) result = %#v, want changed old revision", secondResult)
	}
	if !second.hasState(protocol.StateSyncingRepository) || !second.hasState(protocol.StateEnvironmentBroken) {
		t.Fatalf("Sync(old) states = %v, want syncing_repository and environment_broken", second.statuses())
	}

	assertFetchedRepositoryShape(t, layout.RepoDir(), oldTarget, source, oldCommit)
	assertNoComponentTemporaryDirectories(t, layout)
	for _, path := range []string{layout.UpdateStateFile(), layout.MutationStateFile()} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("transaction file %q = %v, want absent", filepath.Base(path), err)
		}
	}
	verificationStore, err := state.NewStore(t.Context(), layout)
	if err != nil {
		t.Fatalf("state.NewStore(verification) error = %v", err)
	}
	defer func() {
		if err := verificationStore.Close(); err != nil {
			t.Errorf("verification Store.Close() error = %v", err)
		}
	}()
	environment, err := verificationStore.ReadEnvironment(t.Context())
	if err != nil {
		t.Fatalf("ReadEnvironment() error = %v", err)
	}
	if environment.Status != protocol.StateEnvironmentBroken ||
		environment.Broken == nil ||
		environment.Broken.TargetVersion != oldTarget.Version() ||
		environment.Broken.Commit != oldCommit ||
		environment.LastSuccessful.Version != "v0.9.0" {
		t.Fatalf("environment = %#v, want broken old target with preserved lastSuccessful", environment)
	}
	server.assertNoServerErrors(t)
}

type componentSyncLogger struct {
	path string
}

func (l componentSyncLogger) LogPath() string { return l.path }
func (componentSyncLogger) Close() error      { return nil }

func componentServiceSyncRequest(
	layout *config.Layout,
	target Target,
	policy mirror.Policy,
	operationID string,
	emitter WorkspaceEmitter,
) SyncRequest {
	return SyncRequest{
		Target:      target,
		Policy:      policy,
		OperationID: operationID,
		PID:         1234,
		Emitter:     emitter,
		LoggerFactory: func(context.Context, string, string) (OperationLogger, error) {
			return componentSyncLogger{path: filepath.Join(layout.RuntimeLogDir(), "workspace-sync.log")}, nil
		},
		Auditor: componentAuditor{},
		Clock:   time.Now,
	}
}
