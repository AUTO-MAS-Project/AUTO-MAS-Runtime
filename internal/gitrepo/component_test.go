package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
	"golang.org/x/sys/windows"
)

type componentAuditor struct{}

func (componentAuditor) RecordDeletion(context.Context, filesystem.DeleteAuditRecord) error {
	return nil
}

type componentEmitter struct{}

func (componentEmitter) EmitProgress(protocol.ProgressEvent) error { return nil }

type componentFetchOutcome struct {
	result FetchResult
	err    error
}

type componentFileIdentity struct {
	volumeSerialNumber uint32
	fileIndex          uint64
}

type componentProtectedSnapshot struct {
	directory componentFileIdentity
	sentinel  componentFileIdentity
	content   string
}

func TestComponent_GitAcquisitionMatrix(t *testing.T) {
	t.Run("preferred source succeeds with shallow single branch and no tags", func(t *testing.T) {
		repository := newGitFixtureRepository(t,
			gitFixtureCommit{label: "base", version: "v0.9.0"},
			gitFixtureCommit{label: "target", version: "v1.0.0"},
		)
		repository.setBranch(t, "v1.0.0", "target")
		repository.addTag(t, "v1.0.0", "target")
		server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
		source := server.source(t, "origin", "origin", true)
		plan := gitFixturePlan(t, []mirror.Source{source}, "")
		layout := componentLayout(t)
		fetcher, operator := componentFetcher(t, layout, server.caBundle)
		target := componentTarget(t, "v1.0.0")
		operationID := componentOperationID(1)

		result, err := fetcher.Fetch(t.Context(), FetchRequest{
			Plan: plan, Target: target, OperationID: operationID,
		})
		if err != nil {
			var operationErr *Error
			details := map[string]any{}
			if errors.As(err, &operationErr) && operationErr != nil {
				details = operationErr.Details()
			}
			t.Fatalf(
				"Fetch() error = %v, details = %#v, server errors = %v, stats = %#v",
				err,
				details,
				errors.Join(server.snapshotErrors()...),
				server.snapshotStats("origin"),
			)
		}
		if result.Revision.Commit() != repository.hash(t, "target").String() ||
			result.Revision.SourceKey() != "origin" {
			t.Fatalf("Fetch() revision = %#v, want target/origin", result.Revision)
		}
		assertFetchedRepositoryShape(t, result.RepositoryPath, target, source, result.Revision.Commit())
		assertCommitObjectAbsent(t, result.RepositoryPath, repository.hash(t, "base"))
		stats := server.snapshotStats("origin")
		if stats.discoveries != 2 || stats.packs != 1 ||
			len(stats.depths) != 1 || stats.depths[0] != 1 {
			t.Fatalf("server stats = %#v, want discoveries=2 packs=1 depth=1", stats)
		}
		removeComponentUpdate(t, operator, layout, operationID)
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})

	t.Run("lagging preferred source switches to official", func(t *testing.T) {
		lagging := newGitFixtureRepository(t,
			gitFixtureCommit{label: "old", version: "v1.0.0"},
		)
		lagging.setBranch(t, "v1.0.0", "old")
		official := newGitFixtureRepository(t,
			gitFixtureCommit{label: "old", version: "v1.0.0"},
			gitFixtureCommit{label: "target", version: "v2.0.0"},
		)
		official.setBranch(t, "v2.0.0", "target")
		server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{
			"lagging":  lagging,
			"official": official,
		})
		laggingSource := server.source(t, "lagging", "lagging", false)
		officialSource := server.source(t, "official", "official", true)
		plan := gitFixturePlan(
			t,
			[]mirror.Source{laggingSource, officialSource},
			"lagging",
		)
		layout := componentLayout(t)
		fetcher, operator := componentFetcher(t, layout, server.caBundle)
		operationID := componentOperationID(2)
		result, err := fetcher.Fetch(t.Context(), FetchRequest{
			Plan: plan, Target: componentTarget(t, "v2.0.0"), OperationID: operationID,
		})
		if err != nil {
			t.Fatalf("Fetch() error = %v", err)
		}
		if result.Revision.SourceKey() != "official" ||
			result.Revision.Commit() != official.hash(t, "target").String() {
			t.Fatalf("Fetch() revision = %#v, want official target", result.Revision)
		}
		laggingStats := server.snapshotStats("lagging")
		officialStats := server.snapshotStats("official")
		if laggingStats.discoveries != 1 || laggingStats.packs != 0 {
			t.Fatalf("lagging stats = %#v, want discovery only", laggingStats)
		}
		if officialStats.discoveries != 2 || officialStats.packs != 1 {
			t.Fatalf("official stats = %#v, want successful fetch", officialStats)
		}
		removeComponentUpdate(t, operator, layout, operationID)
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})

	t.Run("all sources missing branch", func(t *testing.T) {
		first := newGitFixtureRepository(t, gitFixtureCommit{label: "old", version: "v1.0.0"})
		first.setBranch(t, "v1.0.0", "old")
		second := newGitFixtureRepository(t, gitFixtureCommit{label: "old", version: "v1.0.0"})
		second.setBranch(t, "v1.0.0", "old")
		server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{
			"first":  first,
			"second": second,
		})
		plan := gitFixturePlan(t, []mirror.Source{
			server.source(t, "first", "first", false),
			server.source(t, "second", "second", true),
		}, "first")
		layout := componentLayout(t)
		fetcher, _ := componentFetcher(t, layout, server.caBundle)
		_, err := fetcher.Fetch(t.Context(), FetchRequest{
			Plan: plan, Target: componentTarget(t, "v2.0.0"), OperationID: componentOperationID(3),
		})
		assertComponentErrorCode(t, err, protocol.CodeGitBranchNotFound)
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})

	t.Run("interrupted pack response is cleaned", func(t *testing.T) {
		repository := newGitFixtureRepository(t, gitFixtureCommit{label: "target", version: "v1.0.0"})
		repository.setBranch(t, "v1.0.0", "target")
		server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
		barrier := newGitFixtureBarrier()
		server.setFault("origin", gitFixtureFault{
			response:          barrier,
			interruptResponse: true,
		})
		source := server.source(t, "origin", "origin", true)
		layout := componentLayout(t)
		fetcher, _ := componentFetcher(t, layout, server.caBundle)
		plan := gitFixturePlan(t, []mirror.Source{source}, "")
		target := componentTarget(t, "v1.0.0")
		operationID := componentOperationID(4)
		outcome := make(chan componentFetchOutcome, 1)
		go func() {
			result, err := fetcher.Fetch(t.Context(), FetchRequest{
				Plan: plan, Target: target, OperationID: operationID,
			})
			outcome <- componentFetchOutcome{result: result, err: err}
		}()
		waitForGitFixtureBarrier(t, barrier)
		barrier.releaseRequest()
		result := waitForComponentFetch(t, outcome)
		assertComponentErrorCode(t, result.err, protocol.CodeGitCloneFailed)
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})

	cancellationStages := []struct {
		name  string
		index int
		fault func(*gitFixtureBarrier) gitFixtureFault
	}{
		{
			name:  "cancellation during discovery is cleaned",
			index: 5,
			fault: func(barrier *gitFixtureBarrier) gitFixtureFault {
				return gitFixtureFault{discovery: barrier}
			},
		},
		{
			name:  "cancellation during pack generation is cleaned",
			index: 8,
			fault: func(barrier *gitFixtureBarrier) gitFixtureFault {
				return gitFixtureFault{pack: barrier}
			},
		},
		{
			name:  "cancellation while reading response is cleaned",
			index: 9,
			fault: func(barrier *gitFixtureBarrier) gitFixtureFault {
				return gitFixtureFault{responseRead: barrier}
			},
		},
	}
	for _, test := range cancellationStages {
		t.Run(test.name, func(t *testing.T) {
			repository := newGitFixtureRepository(t, gitFixtureCommit{label: "target", version: "v1.0.0"})
			repository.setBranch(t, "v1.0.0", "target")
			server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
			barrier := newGitFixtureBarrier()
			server.setFault("origin", test.fault(barrier))
			source := server.source(t, "origin", "origin", true)
			layout := componentLayout(t)
			fetcher, _ := componentFetcher(t, layout, server.caBundle)
			plan := gitFixturePlan(t, []mirror.Source{source}, "")
			target := componentTarget(t, "v1.0.0")
			operationID := componentOperationID(test.index)
			ctx, cancel := context.WithCancel(t.Context())
			outcome := make(chan componentFetchOutcome, 1)
			go func() {
				result, err := fetcher.Fetch(ctx, FetchRequest{
					Plan: plan, Target: target, OperationID: operationID,
				})
				outcome <- componentFetchOutcome{result: result, err: err}
			}()
			waitForGitFixtureBarrier(t, barrier)
			cancel()
			result := waitForComponentFetch(t, outcome)
			assertComponentErrorCode(t, result.err, protocol.CodeOperationCancelled)
			assertNoComponentTemporaryDirectories(t, layout)
			server.assertNoServerErrors(t)
		})
	}

	t.Run("same version branch moves to a different commit", func(t *testing.T) {
		repository := newGitFixtureRepository(t,
			gitFixtureCommit{label: "first", version: "v1.0.0"},
			gitFixtureCommit{label: "second", version: "v1.0.0"},
		)
		repository.setBranch(t, "v1.0.0", "first")
		server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
		source := server.source(t, "origin", "origin", true)
		plan := gitFixturePlan(t, []mirror.Source{source}, "")
		layout := componentLayout(t)
		fetcher, operator := componentFetcher(t, layout, server.caBundle)
		target := componentTarget(t, "v1.0.0")

		firstOperation := componentOperationID(6)
		first, err := fetcher.Fetch(t.Context(), FetchRequest{
			Plan: plan, Target: target, OperationID: firstOperation,
		})
		if err != nil {
			t.Fatalf("Fetch(first) error = %v", err)
		}
		if first.Revision.Commit() != repository.hash(t, "first").String() {
			t.Fatalf("first commit = %q, want %q", first.Revision.Commit(), repository.hash(t, "first"))
		}
		removeComponentUpdate(t, operator, layout, firstOperation)

		repository.setBranch(t, "v1.0.0", "second")
		secondOperation := componentOperationID(7)
		second, err := fetcher.Fetch(t.Context(), FetchRequest{
			Plan: plan, Target: target, OperationID: secondOperation,
		})
		if err != nil {
			t.Fatalf("Fetch(second) error = %v", err)
		}
		if second.Revision.Commit() != repository.hash(t, "second").String() ||
			second.Revision.Commit() == first.Revision.Commit() {
			t.Fatalf("second commit = %q, first = %q", second.Revision.Commit(), first.Revision.Commit())
		}
		removeComponentUpdate(t, operator, layout, secondOperation)
		stats := server.snapshotStats("origin")
		if stats.discoveries != 4 || stats.packs != 2 ||
			len(stats.depths) != 2 || stats.depths[0] != 1 || stats.depths[1] != 1 {
			t.Fatalf("server stats = %#v, want two depth-1 fetches", stats)
		}
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})
}

func TestComponent_GitReplacementMatrix(t *testing.T) {
	t.Run("first installation", func(t *testing.T) {
		repository := newGitFixtureRepository(t, gitFixtureCommit{label: "target", version: "v1.0.0"})
		repository.setBranch(t, "v1.0.0", "target")
		server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
		source := server.source(t, "origin", "origin", true)
		plan := gitFixturePlan(t, []mirror.Source{source}, "")
		layout := componentLayout(t)
		fetcher, operator := componentFetcher(t, layout, server.caBundle)
		store := componentStateStore(t, layout)
		target := componentTarget(t, "v1.0.0")

		fetched := componentActivate(t, layout, fetcher, operator, store, plan, target, componentOperationID(20))
		assertFetchedRepositoryShape(t, layout.RepoDir(), target, source, fetched.Revision.Commit())
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})

	t.Run("dirty tracked and untracked files are replaced", func(t *testing.T) {
		repository := newGitFixtureRepository(t,
			gitFixtureCommit{label: "old", version: "v1.0.0"},
			gitFixtureCommit{label: "new", version: "v2.0.0"},
		)
		repository.setBranch(t, "v1.0.0", "old")
		repository.setBranch(t, "v2.0.0", "new")
		server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
		source := server.source(t, "origin", "origin", true)
		plan := gitFixturePlan(t, []mirror.Source{source}, "")
		layout := componentLayout(t)
		fetcher, operator := componentFetcher(t, layout, server.caBundle)
		store := componentStateStore(t, layout)
		oldTarget := componentTarget(t, "v1.0.0")
		newTarget := componentTarget(t, "v2.0.0")

		old := componentActivate(t, layout, fetcher, operator, store, plan, oldTarget, componentOperationID(21))
		tracked := filepath.Join(layout.RepoDir(), "res", "version.json")
		if err := os.WriteFile(tracked, []byte(`{"version":"v1.0.0","dirty":true}`), 0o600); err != nil {
			t.Fatalf("WriteFile(tracked) error = %v", err)
		}
		untracked := filepath.Join(layout.RepoDir(), "untracked-dirty.txt")
		if err := os.WriteFile(untracked, []byte("local change"), 0o600); err != nil {
			t.Fatalf("WriteFile(untracked) error = %v", err)
		}

		newRevision := componentActivate(t, layout, fetcher, operator, store, plan, newTarget, componentOperationID(22))
		if newRevision.Revision.Commit() == old.Revision.Commit() {
			t.Fatal("replacement kept the old commit")
		}
		assertFetchedRepositoryShape(t, layout.RepoDir(), newTarget, source, newRevision.Revision.Commit())
		if _, err := os.Lstat(untracked); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("untracked file remains after replacement: %v", err)
		}
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})

	t.Run("real read-only pack index is removed from retired repository", func(t *testing.T) {
		repository := newGitFixtureRepository(t, gitFixtureCommit{label: "target", version: "v1.0.0"})
		repository.setBranch(t, "v1.0.0", "target")
		server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
		source := server.source(t, "origin", "origin", true)
		layout := componentLayout(t)
		operator := componentOperator(t, layout)
		target := componentTarget(t, "v1.0.0")
		operationID := componentOperationID(36)
		previous := mustRepoPreviousDir(t, layout, operationID)

		_, err := git.PlainCloneContext(t.Context(), previous, false, &git.CloneOptions{
			URL:               source.BaseURL(),
			ReferenceName:     plumbing.NewBranchReferenceName(target.Branch()),
			SingleBranch:      true,
			Depth:             1,
			Tags:              git.NoTags,
			RecurseSubmodules: git.NoRecurseSubmodules,
			CABundle:          append([]byte(nil), server.caBundle...),
		})
		if err != nil {
			t.Fatalf("PlainCloneContext() error = %v", err)
		}
		indexes, err := filepath.Glob(filepath.Join(previous, ".git", "objects", "pack", "*.idx"))
		if err != nil {
			t.Fatalf("Glob(pack indexes) error = %v", err)
		}
		if len(indexes) == 0 {
			t.Fatal("PlainCloneContext() produced no pack index")
		}
		for _, index := range indexes {
			attributes := componentFileAttributes(t, index)
			if attributes&windows.FILE_ATTRIBUTE_READONLY == 0 {
				t.Fatalf("pack index %q attributes = %#x, want FILE_ATTRIBUTE_READONLY", filepath.Base(index), attributes)
			}
		}

		result, err := operator.RemoveTree(t.Context(), filesystem.DeleteRequest{
			Kind:        filesystem.DeleteRepositoryRetired,
			Target:      previous,
			OperationID: operationID,
			Reason:      "component-real-pack-cleanup",
		})
		if err != nil {
			t.Fatalf("RemoveTree(retired) error = %v", err)
		}
		if result.Partial || !result.AuditCompleted {
			t.Fatalf("RemoveTree(retired) result = %#v, want complete audit", result)
		}
		if _, err := os.Lstat(previous); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retired repository remains after cleanup: %v", err)
		}
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})

	t.Run("protected directories survive fetch swap and cleanup", func(t *testing.T) {
		repository := newGitFixtureRepository(t,
			gitFixtureCommit{label: "old", version: "v1.0.0"},
			gitFixtureCommit{label: "new", version: "v2.0.0"},
		)
		repository.setBranch(t, "v1.0.0", "old")
		repository.setBranch(t, "v2.0.0", "new")
		server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
		source := server.source(t, "origin", "origin", true)
		plan := gitFixturePlan(t, []mirror.Source{source}, "")
		layout := componentLayout(t)
		protected := seedComponentProtectedDirectories(t, layout)
		fetcher, operator := componentFetcher(t, layout, server.caBundle)
		store := componentStateStore(t, layout)

		componentActivate(
			t,
			layout,
			fetcher,
			operator,
			store,
			plan,
			componentTarget(t, "v1.0.0"),
			componentOperationID(37),
		)
		assertComponentProtectedDirectories(t, protected)
		activated := componentActivate(
			t,
			layout,
			fetcher,
			operator,
			store,
			plan,
			componentTarget(t, "v2.0.0"),
			componentOperationID(38),
		)
		if activated.Revision.Commit() != repository.hash(t, "new").String() {
			t.Fatalf("activated commit = %q, want new commit", activated.Revision.Commit())
		}
		assertComponentProtectedDirectories(t, protected)
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})

	t.Run("directory handle without delete share maps to occupied", func(t *testing.T) {
		repository := newGitFixtureRepository(t,
			gitFixtureCommit{label: "old", version: "v1.0.0"},
			gitFixtureCommit{label: "new", version: "v2.0.0"},
		)
		repository.setBranch(t, "v1.0.0", "old")
		repository.setBranch(t, "v2.0.0", "new")
		server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
		source := server.source(t, "origin", "origin", true)
		plan := gitFixturePlan(t, []mirror.Source{source}, "")
		layout := componentLayout(t)
		fetcher, operator := componentFetcher(t, layout, server.caBundle)
		store := componentStateStore(t, layout)
		componentActivate(t, layout, fetcher, operator, store, plan, componentTarget(t, "v1.0.0"), componentOperationID(23))

		target := componentTarget(t, "v2.0.0")
		fetched, err := fetcher.Fetch(t.Context(), FetchRequest{
			Plan: plan, Target: target, OperationID: componentOperationID(24),
		})
		if err != nil {
			t.Fatalf("Fetch(new) error = %v", err)
		}
		tx := componentWriteUpdateTransaction(t, store, target, componentOperationID(24))
		handle := componentOpenNoDeleteHandle(t, layout.RepoDir())
		closed := false
		t.Cleanup(func() {
			if !closed {
				_ = windows.CloseHandle(handle)
			}
		})
		swapper, err := NewSwapper(layout, operator, store)
		if err != nil {
			t.Fatalf("NewSwapper() error = %v", err)
		}
		_, err = swapper.Swap(t.Context(), SwapRequest{Transaction: tx, Revision: fetched.Revision})
		assertComponentErrorCode(t, err, protocol.CodeDirectoryOccupied)
		if got, readErr := os.ReadFile(layout.RepoVersionFile()); readErr != nil || !strings.Contains(string(got), "v1.0.0") {
			t.Fatalf("old repository after occupied swap = %q, %v", got, readErr)
		}
		if closeErr := windows.CloseHandle(handle); closeErr != nil {
			t.Fatalf("CloseHandle() error = %v", closeErr)
		}
		closed = true
		removeComponentTransaction(t, store)
		removeComponentUpdate(t, operator, layout, componentOperationID(24))
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})

	t.Run("failed verification preserves complete active repository", func(t *testing.T) {
		repository := newGitFixtureRepository(t,
			gitFixtureCommit{label: "old", version: "v1.0.0"},
			gitFixtureCommit{label: "wrong", version: "v1.0.0"},
		)
		repository.setBranch(t, "v1.0.0", "old")
		repository.setBranch(t, "v2.0.0", "wrong")
		server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
		source := server.source(t, "origin", "origin", true)
		plan := gitFixturePlan(t, []mirror.Source{source}, "")
		layout := componentLayout(t)
		fetcher, operator := componentFetcher(t, layout, server.caBundle)
		store := componentStateStore(t, layout)
		oldTarget := componentTarget(t, "v1.0.0")
		componentActivate(t, layout, fetcher, operator, store, plan, oldTarget, componentOperationID(25))

		_, err := fetcher.Fetch(t.Context(), FetchRequest{
			Plan: plan, Target: componentTarget(t, "v2.0.0"), OperationID: componentOperationID(26),
		})
		assertComponentErrorCode(t, err, protocol.CodeGitVersionMismatch)
		assertFetchedRepositoryShape(t, layout.RepoDir(), oldTarget, source, repository.hash(t, "old").String())
		assertNoComponentTemporaryDirectories(t, layout)
		server.assertNoServerErrors(t)
	})
}

func TestComponent_VersionDowngradeUsesSameFlow(t *testing.T) {
	repository := newGitFixtureRepository(t,
		gitFixtureCommit{label: "old", version: "v1.0.0"},
		gitFixtureCommit{label: "new", version: "v2.0.0"},
	)
	repository.setBranch(t, "v1.0.0", "old")
	repository.setBranch(t, "v2.0.0", "new")
	server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
	source := server.source(t, "origin", "origin", true)
	plan := gitFixturePlan(t, []mirror.Source{source}, "")
	layout := componentLayout(t)
	fetcher, operator := componentFetcher(t, layout, server.caBundle)
	store := componentStateStore(t, layout)

	newTarget := componentTarget(t, "v2.0.0")
	oldTarget := componentTarget(t, "v1.0.0")
	componentActivate(t, layout, fetcher, operator, store, plan, newTarget, componentOperationID(27))
	downgraded := componentActivate(t, layout, fetcher, operator, store, plan, oldTarget, componentOperationID(28))
	if downgraded.Revision.Commit() != repository.hash(t, "old").String() {
		t.Fatalf("downgraded commit = %q, want old commit", downgraded.Revision.Commit())
	}
	assertFetchedRepositoryShape(t, layout.RepoDir(), oldTarget, source, downgraded.Revision.Commit())
	assertNoComponentTemporaryDirectories(t, layout)
	server.assertNoServerErrors(t)
}

func TestComponent_NoTemporaryDirectoriesRemain(t *testing.T) {
	repository := newGitFixtureRepository(t, gitFixtureCommit{label: "target", version: "v1.0.0"})
	repository.setBranch(t, "v1.0.0", "target")
	server := newGitHTTPSFixture(t, map[string]*gitFixtureRepository{"origin": repository})
	source := server.source(t, "origin", "origin", true)
	plan := gitFixturePlan(t, []mirror.Source{source}, "")
	layout := componentLayout(t)
	fetcher, operator := componentFetcher(t, layout, server.caBundle)
	store := componentStateStore(t, layout)
	target := componentTarget(t, "v1.0.0")
	componentActivate(t, layout, fetcher, operator, store, plan, target, componentOperationID(29))
	assertNoComponentTemporaryDirectories(t, layout)

	missing := componentTarget(t, "v2.0.0")
	_, err := fetcher.Fetch(t.Context(), FetchRequest{
		Plan: plan, Target: missing, OperationID: componentOperationID(30),
	})
	assertComponentErrorCode(t, err, protocol.CodeGitBranchNotFound)
	assertNoComponentTemporaryDirectories(t, layout)
	server.assertNoServerErrors(t)
}

func TestComponent_GitRecoveryMatrix(t *testing.T) {
	t.Run("clone half complete is removed", func(t *testing.T) {
		layout, operator, store, recovery, tx := componentRecoveryFixture(t, componentOperationID(31), protocol.StageWorkspaceClone, "v1.0.0")
		update := mustRepoUpdateDir(t, layout, tx.OperationID)
		if err := os.MkdirAll(filepath.Join(update, ".git", "objects"), 0o700); err != nil {
			t.Fatalf("MkdirAll(partial) error = %v", err)
		}
		writeSwapMarker(t, update, "partial", "clone")
		result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
		if err != nil || !result.Recovered || !result.TransactionRemoved {
			t.Fatalf("Recover() = %#v, %v, want cleaned transaction", result, err)
		}
		if _, err := os.Lstat(update); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial update remains: %v", err)
		}
		assertNoComponentTemporaryDirectories(t, layout)
		_ = operator
		_ = store
	})

	t.Run("verified repository before swap is removed", func(t *testing.T) {
		layout, operator, store, recovery, tx := componentRecoveryFixture(t, componentOperationID(32), protocol.StageWorkspaceVerify, "v2.0.0")
		update := mustRepoUpdateDir(t, layout, tx.OperationID)
		writeRecoveryRepository(t, update, "v2.0.0", recoverySourceURL(t), "target")
		writeRecoveryRepository(t, layout.RepoDir(), "v1.0.0", recoverySourceURL(t), "old")
		result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
		if err != nil || !result.Recovered {
			t.Fatalf("Recover() = %#v, %v, want verified update cleanup", result, err)
		}
		assertSwapMarker(t, filepath.Join(layout.RepoDir(), "marker-old"), "old")
		if _, err := os.Lstat(update); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("verified update remains: %v", err)
		}
		assertNoComponentTemporaryDirectories(t, layout)
		_ = operator
		_ = store
	})

	t.Run("between renames rolls previous back", func(t *testing.T) {
		layout, operator, store, recovery, tx := componentRecoveryFixture(t, componentOperationID(33), protocol.StageWorkspaceSwap, "v2.0.0")
		previous := mustRepoPreviousDir(t, layout, tx.OperationID)
		update := mustRepoUpdateDir(t, layout, tx.OperationID)
		writeRecoveryRepository(t, previous, "v1.0.0", recoverySourceURL(t), "old")
		writeRecoveryRepository(t, update, "v2.0.0", recoverySourceURL(t), "target")
		result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
		if err != nil || !result.Recovered || !result.MutationApplied {
			t.Fatalf("Recover() = %#v, %v, want rollback", result, err)
		}
		assertSwapMarker(t, filepath.Join(layout.RepoDir(), "marker-old"), "old")
		if _, err := os.Lstat(previous); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("previous remains after rollback: %v", err)
		}
		if _, err := os.Lstat(update); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("update remains after rollback: %v", err)
		}
		assertNoComponentTemporaryDirectories(t, layout)
		_ = operator
		_ = store
	})

	t.Run("active target writes missing environment", func(t *testing.T) {
		layout, operator, store, recovery, _ := componentRecoveryFixture(t, componentOperationID(34), protocol.StageWorkspaceCleanup, "v2.0.0")
		writeRecoveryRepository(t, layout.RepoDir(), "v2.0.0", recoverySourceURL(t), "target")
		result, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
		if err != nil || !result.Recovered || !result.EnvironmentWritten {
			t.Fatalf("Recover() = %#v, %v, want environment write", result, err)
		}
		environment, err := store.ReadEnvironment(t.Context())
		if err != nil || environment.Status != protocol.StateEnvironmentBroken || environment.Broken == nil {
			t.Fatalf("environment = %#v, error = %v, want repository_changed", environment, err)
		}
		if environment.Broken.Reason != state.ReasonRepositoryChanged || environment.Broken.TargetVersion != "v2.0.0" {
			t.Fatalf("environment broken = %#v, want target repository_changed", environment.Broken)
		}
		assertNoComponentTemporaryDirectories(t, layout)
		_ = operator
	})

	t.Run("corrupt transaction has no directory side effects", func(t *testing.T) {
		layout := componentLayout(t)
		if err := os.MkdirAll(layout.AppRoot(), 0o700); err != nil {
			t.Fatalf("MkdirAll(app-root) error = %v", err)
		}
		if err := os.MkdirAll(layout.StateDir(), 0o700); err != nil {
			t.Fatalf("MkdirAll(state) error = %v", err)
		}
		writeRecoveryRepository(t, layout.RepoDir(), "v1.0.0", recoverySourceURL(t), "untouched")
		if err := os.WriteFile(layout.UpdateStateFile(), []byte(`{"schemaVersion":1,"operationId":"bad"}`), 0o600); err != nil {
			t.Fatalf("WriteFile(update) error = %v", err)
		}
		operator := componentOperator(t, layout)
		store := componentStateStore(t, layout)
		recovery, err := NewRecovery(layout, operator, store)
		if err != nil {
			t.Fatalf("NewRecovery() error = %v", err)
		}
		_, err = recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
		assertComponentErrorCode(t, err, protocol.CodeUpdateStateAmbiguous)
		assertSwapMarker(t, filepath.Join(layout.RepoDir(), "marker-untouched"), "untouched")
		if _, err := os.Lstat(layout.UpdateStateFile()); err != nil {
			t.Fatalf("corrupt transaction was removed: %v", err)
		}
	})

	t.Run("same version different commits is ambiguous", func(t *testing.T) {
		layout, operator, store, recovery, tx := componentRecoveryFixture(t, componentOperationID(35), protocol.StageWorkspaceVerify, "v1.0.0")
		update := mustRepoUpdateDir(t, layout, tx.OperationID)
		writeRecoveryRepository(t, layout.RepoDir(), "v1.0.0", recoverySourceURL(t), "first")
		writeRecoveryRepositoryWithExtraCommit(t, update, "v1.0.0", recoverySourceURL(t), "second")
		_, err := recovery.Recover(t.Context(), RecoveryRequest{LogPath: recoveryLogPath(t, layout)})
		assertComponentErrorCode(t, err, protocol.CodeUpdateStateAmbiguous)
		assertSwapMarker(t, filepath.Join(layout.RepoDir(), "marker-first"), "first")
		assertSwapMarker(t, filepath.Join(update, "marker-second"), "second")
		if _, err := os.Lstat(layout.UpdateStateFile()); err != nil {
			t.Fatalf("ambiguous transaction was removed: %v", err)
		}
		_ = operator
		_ = store
	})
}

func componentStateStore(t *testing.T, layout *config.Layout) *state.Store {
	t.Helper()
	if err := os.MkdirAll(layout.AppRoot(), 0o700); err != nil {
		t.Fatalf("MkdirAll(app-root) error = %v", err)
	}
	store, err := state.NewStore(t.Context(), layout)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Store.Close() error = %v", err)
		}
	})
	return store
}

func componentWriteUpdateTransaction(
	t *testing.T,
	store *state.Store,
	target Target,
	operationID string,
) state.TransactionState {
	t.Helper()
	tx, err := store.NewTransaction(state.TransactionUpdate, state.TransactionInput{
		OperationID:   operationID,
		Command:       "workspace sync",
		PID:           uint32(os.Getpid()),
		TargetVersion: target.Version(),
		Stage:         protocol.StageWorkspaceVerify,
	})
	if err != nil {
		t.Fatalf("NewTransaction() error = %v", err)
	}
	if err := store.WriteTransaction(t.Context(), state.TransactionUpdate, tx); err != nil {
		t.Fatalf("WriteTransaction() error = %v", err)
	}
	return tx
}

func removeComponentTransaction(t *testing.T, store *state.Store) {
	t.Helper()
	snapshot, err := store.ReadTransaction(t.Context(), state.TransactionUpdate)
	if errors.Is(err, state.ErrNotFound) {
		return
	}
	if err != nil {
		t.Fatalf("ReadTransaction() error = %v", err)
	}
	if err := store.RemoveTransaction(t.Context(), snapshot); err != nil {
		t.Fatalf("RemoveTransaction() error = %v", err)
	}
}

func componentActivate(
	t *testing.T,
	layout *config.Layout,
	fetcher *Fetcher,
	operator *filesystem.Operator,
	store *state.Store,
	plan mirror.Plan,
	target Target,
	operationID string,
) FetchResult {
	t.Helper()
	fetched, err := fetcher.Fetch(t.Context(), FetchRequest{
		Plan: plan, Target: target, OperationID: operationID,
	})
	if err != nil {
		t.Fatalf("Fetch(%s) error = %v", target.Version(), err)
	}
	tx := componentWriteUpdateTransaction(t, store, target, operationID)
	swapper, err := NewSwapper(layout, operator, store)
	if err != nil {
		t.Fatalf("NewSwapper() error = %v", err)
	}
	result, err := swapper.Swap(t.Context(), SwapRequest{Transaction: tx, Revision: fetched.Revision})
	if err != nil {
		t.Fatalf("Swap(%s) error = %v", target.Version(), err)
	}
	if !result.RepositoryActivated || !result.CleanupCompleted {
		t.Fatalf("Swap(%s) result = %#v, want active and clean", target.Version(), result)
	}
	removeComponentTransaction(t, store)
	return fetched
}

func componentRecoveryFixture(
	t *testing.T,
	operationID string,
	stage protocol.Stage,
	version string,
) (*config.Layout, *filesystem.Operator, *state.Store, *Recovery, state.TransactionState) {
	t.Helper()
	layout := componentLayout(t)
	operator := componentOperator(t, layout)
	store := componentStateStore(t, layout)
	tx, err := store.NewTransaction(state.TransactionUpdate, state.TransactionInput{
		OperationID:   operationID,
		Command:       "workspace sync",
		PID:           uint32(os.Getpid()),
		TargetVersion: version,
		Stage:         stage,
	})
	if err != nil {
		t.Fatalf("NewTransaction() error = %v", err)
	}
	if err := store.WriteTransaction(t.Context(), state.TransactionUpdate, tx); err != nil {
		t.Fatalf("WriteTransaction() error = %v", err)
	}
	recovery, err := NewRecovery(layout, operator, store)
	if err != nil {
		t.Fatalf("NewRecovery() error = %v", err)
	}
	return layout, operator, store, recovery, tx
}

func componentOpenNoDeleteHandle(t *testing.T, path string) windows.Handle {
	t.Helper()
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString() error = %v", err)
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile(directory) error = %v", err)
	}
	return handle
}

func seedComponentProtectedDirectories(
	t *testing.T,
	layout *config.Layout,
) map[string]componentProtectedSnapshot {
	t.Helper()
	paths := []string{
		layout.ConfigDir(),
		layout.DataDir(),
		layout.HistoryDir(),
		layout.ScriptDir(),
		layout.DebugDir(),
		layout.PluginsDir(),
		layout.LogsDir(),
		layout.RuntimeLogDir(),
		layout.RuntimeDir(),
	}
	snapshots := make(map[string]componentProtectedSnapshot, len(paths))
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", filepath.Base(path), err)
		}
		sentinel := filepath.Join(path, "m4-protected-sentinel.txt")
		content := "protected:" + filepath.Base(path)
		if err := os.WriteFile(sentinel, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", filepath.Base(path), err)
		}
		snapshots[path] = componentProtectedSnapshot{
			directory: componentPathIdentity(t, path),
			sentinel:  componentPathIdentity(t, sentinel),
			content:   content,
		}
	}
	return snapshots
}

func assertComponentProtectedDirectories(
	t *testing.T,
	want map[string]componentProtectedSnapshot,
) {
	t.Helper()
	for path, snapshot := range want {
		directory := componentPathIdentity(t, path)
		if directory != snapshot.directory {
			t.Fatalf("protected directory %q identity = %#v, want %#v", filepath.Base(path), directory, snapshot.directory)
		}
		sentinelPath := filepath.Join(path, "m4-protected-sentinel.txt")
		sentinel := componentPathIdentity(t, sentinelPath)
		if sentinel != snapshot.sentinel {
			t.Fatalf("protected sentinel %q identity = %#v, want %#v", filepath.Base(path), sentinel, snapshot.sentinel)
		}
		content, err := os.ReadFile(sentinelPath)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", filepath.Base(path), err)
		}
		if string(content) != snapshot.content {
			t.Fatalf("protected sentinel %q content = %q, want %q", filepath.Base(path), content, snapshot.content)
		}
	}
}

func componentPathIdentity(t *testing.T, path string) componentFileIdentity {
	t.Helper()
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(%q) error = %v", filepath.Base(path), err)
	}
	handle, err := windows.CreateFile(
		pathUTF16,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		t.Fatalf("CreateFile(%q) error = %v", filepath.Base(path), err)
	}
	var information windows.ByHandleFileInformation
	informationErr := windows.GetFileInformationByHandle(handle, &information)
	closeErr := windows.CloseHandle(handle)
	if informationErr != nil {
		t.Fatalf("GetFileInformationByHandle(%q) error = %v", filepath.Base(path), informationErr)
	}
	if closeErr != nil {
		t.Fatalf("CloseHandle(%q) error = %v", filepath.Base(path), closeErr)
	}
	return componentFileIdentity{
		volumeSerialNumber: information.VolumeSerialNumber,
		fileIndex: uint64(information.FileIndexHigh)<<32 |
			uint64(information.FileIndexLow),
	}
}

func componentFileAttributes(t *testing.T, path string) uint32 {
	t.Helper()
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(%q) error = %v", filepath.Base(path), err)
	}
	attributes, err := windows.GetFileAttributes(pathUTF16)
	if err != nil {
		t.Fatalf("GetFileAttributes(%q) error = %v", filepath.Base(path), err)
	}
	return attributes
}

func componentLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	return layout
}

func componentOperator(
	t *testing.T,
	layout *config.Layout,
) *filesystem.Operator {
	t.Helper()
	operator, err := filesystem.New(
		t.Context(),
		layout,
		componentAuditor{},
		filesystem.WithWait(func(ctx context.Context, _ time.Duration) error {
			return ctx.Err()
		}),
		filesystem.WithRenameDelays(time.Nanosecond),
	)
	if err != nil {
		t.Fatalf("filesystem.New() error = %v", err)
	}
	return operator
}

func componentFetcher(
	t *testing.T,
	layout *config.Layout,
	caBundle []byte,
) (*Fetcher, *filesystem.Operator) {
	t.Helper()
	operator := componentOperator(t, layout)
	rotator, err := mirror.NewRotator(
		mirror.WithMaxSourceAttempts(1),
		mirror.WithRotatorWait(func(ctx context.Context, _ time.Duration) error {
			return ctx.Err()
		}),
		mirror.WithRetryDelay(time.Nanosecond),
	)
	if err != nil {
		t.Fatalf("NewRotator() error = %v", err)
	}
	fetcher, err := newFetcherWithDependencies(fetcherDependencies{
		layout:       layout,
		rotator:      rotator,
		git:          goGitClient{},
		remover:      operator,
		verifier:     NewVerifier(),
		prepareDir:   prepareManagedDirectoryLease,
		emitProgress: componentEmitter{}.EmitProgress,
		caBundle:     caBundle,
	})
	if err != nil {
		t.Fatalf("newFetcherWithDependencies() error = %v", err)
	}
	return fetcher, operator
}

func componentTarget(t *testing.T, version string) Target {
	t.Helper()
	target, err := ParseTarget(version)
	if err != nil {
		t.Fatalf("ParseTarget(%q) error = %v", version, err)
	}
	return target
}

func componentOperationID(index int) string {
	return fmt.Sprintf("01J%023d", index)
}

func waitForComponentFetch(
	t *testing.T,
	outcome <-chan componentFetchOutcome,
) componentFetchOutcome {
	t.Helper()
	select {
	case result := <-outcome:
		return result
	case <-time.After(gitFixtureTimeout):
		t.Fatal("timed out waiting for component fetch")
		return componentFetchOutcome{}
	}
}

func assertComponentErrorCode(
	t *testing.T,
	err error,
	want protocol.Code,
) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %s", want)
	}
	var operationErr *Error
	if !errors.As(err, &operationErr) || operationErr.Code() != want {
		t.Fatalf("error = %v, want code %s", err, want)
	}
}

func assertFetchedRepositoryShape(
	t *testing.T,
	path string,
	target Target,
	source mirror.Source,
	commit string,
) {
	t.Helper()
	snapshot, err := (goGitRepositoryReader{}).Inspect(t.Context(), path)
	if err != nil {
		t.Fatalf("Inspect() error = %v", err)
	}
	if !snapshot.nonBare || !snapshot.headSymbolic ||
		snapshot.headTarget != plumbing.NewBranchReferenceName(target.Branch()).String() ||
		snapshot.commit != commit || !containsString(snapshot.shallow, commit) ||
		len(snapshot.tags) != 0 || len(snapshot.remotes) != 1 ||
		len(snapshot.remotes[0].fetchURLs) != 1 ||
		snapshot.remotes[0].fetchURLs[0] != source.BaseURL() {
		t.Fatalf("repository snapshot = %#v, want shallow target identity", snapshot)
	}
	version, err := parseRepositoryVersion(snapshot.versionPayload)
	if err != nil || version != target.Version() {
		t.Fatalf("repository version = %q, error = %v, want %q", version, err, target.Version())
	}
	repository, err := git.PlainOpen(path)
	if err != nil {
		t.Fatalf("PlainOpen() error = %v", err)
	}
	branches, err := repository.Branches()
	if err != nil {
		t.Fatalf("Branches() error = %v", err)
	}
	var branchNames []string
	for {
		reference, nextErr := branches.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			branches.Close()
			t.Fatalf("Branches().Next() error = %v", nextErr)
		}
		branchNames = append(branchNames, reference.Name().String())
	}
	branches.Close()
	if len(branchNames) != 1 || branchNames[0] != plumbing.NewBranchReferenceName(target.Branch()).String() {
		t.Fatalf("local branches = %v, want only target branch", branchNames)
	}
}

func assertCommitObjectAbsent(t *testing.T, repositoryPath string, hash plumbing.Hash) {
	t.Helper()
	repository, err := git.PlainOpen(repositoryPath)
	if err != nil {
		t.Fatalf("PlainOpen() error = %v", err)
	}
	if _, err := repository.CommitObject(hash); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Fatalf("CommitObject(%s) error = %v, want object absent from depth-1 clone", hash, err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func removeComponentUpdate(
	t *testing.T,
	operator *filesystem.Operator,
	layout *config.Layout,
	operationID string,
) {
	t.Helper()
	path, err := layout.RepoUpdateDir(operationID)
	if err != nil {
		t.Fatalf("RepoUpdateDir() error = %v", err)
	}
	result, err := operator.RemoveTree(t.Context(), filesystem.DeleteRequest{
		Kind:        filesystem.DeleteRepositoryUpdate,
		Target:      path,
		OperationID: operationID,
		Reason:      "component-test-cleanup",
	})
	if err != nil {
		t.Fatalf("RemoveTree(update) error = %v", err)
	}
	if result.Partial || !result.AuditCompleted {
		t.Fatalf("RemoveTree(update) result = %#v, want complete audit", result)
	}
}

func assertNoComponentTemporaryDirectories(t *testing.T, layout *config.Layout) {
	t.Helper()
	entries, err := os.ReadDir(layout.AppRoot())
	if err != nil {
		t.Fatalf("ReadDir(app-root) error = %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "repo.update-") ||
			strings.HasPrefix(entry.Name(), "repo.previous-") {
			t.Fatalf("temporary repository directory remains: %q", entry.Name())
		}
	}
}
