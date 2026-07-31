package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

type deleteAuditorFunc func(context.Context, DeleteAuditRecord) error

type removeTreeContextKey struct{}

func (f deleteAuditorFunc) RecordDeletion(
	ctx context.Context,
	record DeleteAuditRecord,
) error {
	return f(ctx, record)
}

func TestRemoveTree_RemovesManagedTreeByPinnedHandles(t *testing.T) {
	operator, layout, auditor := newRemoveTreeFixture(t)
	target := layout.BuildCacheDir()
	if err := os.MkdirAll(filepath.Join(target, "nested"), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "nested", "file"), []byte("x"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	result, err := operator.RemoveTree(t.Context(), buildCacheDeleteRequest(layout))
	if err != nil || !result.Removed || result.Partial || !result.AuditCompleted {
		t.Fatalf("RemoveTree() = %#v, %v", result, err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target still exists: %v", err)
	}
	assertAuditPhases(t, auditor.records, "succeeded")
}

func TestRemoveTree_RejectsReparseWithoutDeletingLink(t *testing.T) {
	operator, layout, _ := newRemoveTreeFixture(t)
	external := t.TempDir()
	sentinel := filepath.Join(external, "sentinel")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	link := filepath.Join(layout.BuildCacheDir(), "escape")
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	result, err := operator.RemoveTree(t.Context(), buildCacheDeleteRequest(layout))
	if result.Removed || result.Partial {
		t.Fatalf("RemoveTree() result = %#v, want no mutation", result)
	}
	assertFilesystemCode(t, err, protocol.CodeUnsafeReparsePoint)
	got, _ := os.ReadFile(sentinel)
	if string(got) != "unchanged" {
		t.Fatalf("external sentinel = %q, want unchanged", got)
	}
}

func TestRemoveTree_TraversalPinsChildBeforeDescent(t *testing.T) {
	testRemoveTreeTraversalPinsChildBeforeDescent(t)
}

func testRemoveTreeTraversalPinsChildBeforeDescent(t *testing.T) {
	t.Helper()
	operator, layout, _ := newRemoveTreeFixture(t)
	child := filepath.Join(layout.BuildCacheDir(), "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	openRelative := operator.api.openRelative
	entered := make(chan struct{})
	release := make(chan struct{})
	operator.api.openRelative = func(
		parent windows.Handle,
		name string,
		spec openSpec,
	) (windows.Handle, error) {
		handle, err := openRelative(parent, name, spec)
		if err == nil && name == "child" {
			close(entered)
			<-release
		}
		return handle, err
	}
	done := make(chan error, 1)
	go func() {
		_, err := operator.RemoveTree(t.Context(), buildCacheDeleteRequest(layout))
		done <- err
	}()
	<-entered
	if err := os.Rename(child, child+"-other"); err == nil {
		close(release)
		t.Fatal("child renamed after open and before descent")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("RemoveTree() error = %v", err)
	}
}

func TestRemoveTree_PartialFailureReportsActualMutation(t *testing.T) {
	operator, layout, _ := newRemoveTreeFixture(t)
	for _, name := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(layout.BuildCacheDir(), name), []byte(name), 0o600); err != nil {
			t.Fatalf("os.WriteFile() error = %v", err)
		}
	}
	injected := errors.New("second disposition failed")
	setDisposition := operator.api.setDisposition
	calls := 0
	operator.api.setDisposition = func(handle windows.Handle) error {
		calls++
		if calls == 2 {
			return injected
		}
		return setDisposition(handle)
	}
	result, err := operator.RemoveTree(t.Context(), buildCacheDeleteRequest(layout))
	if !result.Removed || !result.Partial || !errors.Is(err, injected) {
		t.Fatalf("RemoveTree() = %#v, %v, want removed/partial/injected", result, err)
	}
}

func TestRemoveTree_CancellationStopsNewMutations(t *testing.T) {
	operator, layout, auditor := newRemoveTreeFixture(t)
	for _, name := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(layout.BuildCacheDir(), name), []byte(name), 0o600); err != nil {
			t.Fatalf("os.WriteFile() error = %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	setDisposition := operator.api.setDisposition
	calls := 0
	operator.api.setDisposition = func(handle windows.Handle) error {
		calls++
		err := setDisposition(handle)
		if calls == 1 && err == nil {
			cancel()
		}
		return err
	}
	result, err := operator.RemoveTree(ctx, buildCacheDeleteRequest(layout))
	if !result.Removed || !result.Partial || !errors.Is(err, context.Canceled) {
		t.Fatalf("RemoveTree() = %#v, %v", result, err)
	}
	if calls != 1 {
		t.Fatalf("disposition calls = %d, want 1", calls)
	}
	assertAuditPhases(t, auditor.records, "cancelled")
}

func TestRemoveTree_AuditsSucceededNotFoundFailedAndCancelled(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		layout := newDeleteTestLayout(t)
		auditor := &recordingDeleteAuditor{}
		operator, err := New(t.Context(), layout, auditor)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		result, err := operator.RemoveTree(t.Context(), buildCacheDeleteRequest(layout))
		if err != nil || result.Removed || !result.AuditCompleted {
			t.Fatalf("RemoveTree() = %#v, %v", result, err)
		}
		assertAuditPhases(t, auditor.records, "not_found")
	})
	t.Run("failed", func(t *testing.T) {
		operator, layout, auditor := newRemoveTreeFixture(t)
		injected := errors.New("remove failed")
		operator.api.setDisposition = func(windows.Handle) error { return injected }
		result, err := operator.RemoveTree(t.Context(), buildCacheDeleteRequest(layout))
		if result.Removed || !errors.Is(err, injected) {
			t.Fatalf("RemoveTree() = %#v, %v", result, err)
		}
		assertAuditPhases(t, auditor.records, "failed")
	})
}

func TestRemoveTree_UsesIndependentFinishedContext(t *testing.T) {
	layout := newDeleteTestLayout(t)
	if err := os.MkdirAll(layout.BuildCacheDir(), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	key := removeTreeContextKey{}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), key, "value"))
	var finishedContextErr error
	var finishedContextValue any
	auditor := deleteAuditorFunc(func(recordCtx context.Context, record DeleteAuditRecord) error {
		if record.Phase == DeleteAuditFinished {
			finishedContextErr = recordCtx.Err()
			finishedContextValue = recordCtx.Value(key)
		}
		return nil
	})
	operator, err := New(ctx, layout, auditor)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	setDisposition := operator.api.setDisposition
	operator.api.setDisposition = func(handle windows.Handle) error {
		err := setDisposition(handle)
		if err == nil {
			cancel()
		}
		return err
	}
	result, err := operator.RemoveTree(ctx, buildCacheDeleteRequest(layout))
	if err != nil || !result.Removed {
		t.Fatalf("RemoveTree() = %#v, %v", result, err)
	}
	if finishedContextErr != nil || finishedContextValue != "value" {
		t.Fatalf(
			"finished context error/value = %v/%#v, want nil/value",
			finishedContextErr,
			finishedContextValue,
		)
	}
}

func TestRemoveTree_PreservesDeleteAndFinishedAuditErrors(t *testing.T) {
	layout := newDeleteTestLayout(t)
	if err := os.MkdirAll(layout.BuildCacheDir(), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	deleteErr := errors.New("delete failed")
	auditErr := errors.New("finished audit failed")
	auditor := deleteAuditorFunc(func(_ context.Context, record DeleteAuditRecord) error {
		if record.Phase == DeleteAuditFinished {
			return auditErr
		}
		return nil
	})
	operator, err := New(t.Context(), layout, auditor)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	operator.api.setDisposition = func(windows.Handle) error { return deleteErr }
	_, err = operator.RemoveTree(t.Context(), buildCacheDeleteRequest(layout))
	if !errors.Is(err, deleteErr) || !errors.Is(err, auditErr) {
		t.Fatalf("RemoveTree() error = %v, want both causes", err)
	}
}

func TestRemoveTree_RejectsAppRootRepoVolumeRootAndOutside(t *testing.T) {
	fixture := newDeleteFixture(t)
	for _, target := range []string{
		fixture.layout.AppRoot(),
		fixture.layout.RepoDir(),
		filepath.VolumeName(fixture.layout.AppRoot()) + `\`,
		t.TempDir(),
	} {
		request := buildCacheDeleteRequest(fixture.layout)
		request.Target = target
		result, err := fixture.operator.RemoveTree(t.Context(), request)
		if result.Removed {
			t.Fatalf("RemoveTree(%q) removed a protected target", target)
		}
		assertPathOutside(t, err)
	}
}

func TestRemoveTree_RejectsProtectedRootsAndDescendants(t *testing.T) {
	fixture := newDeleteFixture(t)
	for _, target := range []string{
		fixture.layout.ConfigDir(),
		filepath.Join(fixture.layout.DataDir(), "child"),
		fixture.layout.LogsDir(),
		filepath.Join(fixture.layout.StateDir(), "child"),
	} {
		request := buildCacheDeleteRequest(fixture.layout)
		request.Target = target
		result, err := fixture.operator.RemoveTree(t.Context(), request)
		if result.Removed {
			t.Fatalf("RemoveTree(%q) removed a protected target", target)
		}
		assertPathOutside(t, err)
	}
}

func TestRemoveTree_NotFoundStillRequiresValidIdentity(t *testing.T) {
	fixture := newDeleteFixture(t)
	request := buildCacheDeleteRequest(fixture.layout)
	request.Target = filepath.Join(fixture.layout.AppRoot(), "wrong-missing")
	result, err := fixture.operator.RemoveTree(t.Context(), request)
	if result.Removed {
		t.Fatal("RemoveTree() removed an invalid missing target")
	}
	assertPathOutside(t, err)
}

func TestRemoveTree_StartedFailurePreventsMutation(t *testing.T) {
	layout := newDeleteTestLayout(t)
	if err := os.MkdirAll(layout.BuildCacheDir(), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	injected := errors.New("started audit failed")
	auditor := &recordingDeleteAuditor{err: injected}
	operator, err := New(t.Context(), layout, auditor)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	result, err := operator.RemoveTree(t.Context(), buildCacheDeleteRequest(layout))
	if result.Removed || !errors.Is(err, injected) {
		t.Fatalf("RemoveTree() = %#v, %v", result, err)
	}
	if _, err := os.Stat(layout.BuildCacheDir()); err != nil {
		t.Fatalf("target changed after started failure: %v", err)
	}
}

func TestRemoveTree_ContextsRejectedBeforeIO(t *testing.T) {
	operator, layout, auditor := newRemoveTreeFixture(t)
	dispositionCalls := 0
	operator.api.setDisposition = func(windows.Handle) error {
		dispositionCalls++
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := operator.RemoveTree(ctx, buildCacheDeleteRequest(layout)); !errors.Is(err, context.Canceled) {
		t.Fatalf("RemoveTree() error = %v", err)
	}
	if _, err := operator.RemoveTree(nil, buildCacheDeleteRequest(layout)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("RemoveTree(nil) error = %v", err)
	}
	if dispositionCalls != 0 || len(auditor.records) != 0 {
		t.Fatalf("calls = disposition %d/audit %d, want 0/0", dispositionCalls, len(auditor.records))
	}
}

func TestRemoveTree_CancellationAfterDispositionKeepsApplied(t *testing.T) {
	operator, layout, _ := newRemoveTreeFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	setDisposition := operator.api.setDisposition
	operator.api.setDisposition = func(handle windows.Handle) error {
		err := setDisposition(handle)
		if err == nil {
			cancel()
		}
		return err
	}
	result, _ := operator.RemoveTree(ctx, buildCacheDeleteRequest(layout))
	if !result.Removed {
		t.Fatalf("RemoveTree() result = %#v, want Removed", result)
	}
}

func TestRemoveTree_MatchesAccessMatrixAndParentIdentity(t *testing.T) {
	operator, layout, _ := newRemoveTreeFixture(t)
	if err := os.WriteFile(filepath.Join(layout.BuildCacheDir(), "file"), []byte("x"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	var specs []openSpec
	openRelative := operator.api.openRelative
	operator.api.openRelative = func(
		parent windows.Handle,
		name string,
		spec openSpec,
	) (windows.Handle, error) {
		specs = append(specs, spec)
		return openRelative(parent, name, spec)
	}
	if _, err := operator.RemoveTree(t.Context(), buildCacheDeleteRequest(layout)); err != nil {
		t.Fatalf("RemoveTree() error = %v", err)
	}
	if !containsOpenSpec(specs, recursiveDeleteSpec(false)) ||
		!containsOpenSpec(specs, recursiveDeleteSpec(true)) {
		t.Fatalf("open specs = %#v, want file and directory delete specs", specs)
	}
}

func TestWindows_RemoveTreePinsChildAcrossReplacementRace(t *testing.T) {
	testRemoveTreeTraversalPinsChildBeforeDescent(t)
}

func TestAuditError_MutationAppliedAndUnwrap(t *testing.T) {
	cause := errors.New("audit failed")
	err := &AuditError{
		Phase:           DeleteAuditFinished,
		MutationApplied: true,
		Cause:           cause,
	}
	if !errors.Is(err, cause) || !err.MutationApplied || err.Phase != DeleteAuditFinished {
		t.Fatalf("AuditError = %#v, want finished/applied/cause", err)
	}
}

func newRemoveTreeFixture(
	t *testing.T,
) (*Operator, *config.Layout, *recordingDeleteAuditor) {
	t.Helper()
	layout := newDeleteTestLayout(t)
	if err := os.MkdirAll(layout.BuildCacheDir(), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	auditor := &recordingDeleteAuditor{}
	operator, err := New(t.Context(), layout, auditor)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return operator, layout, auditor
}

func buildCacheDeleteRequest(layout *config.Layout) DeleteRequest {
	return DeleteRequest{
		Kind:        DeleteBuildCache,
		Target:      layout.BuildCacheDir(),
		OperationID: "operation-a",
		Reason:      "test cleanup",
	}
}

func assertAuditPhases(
	t *testing.T,
	records []DeleteAuditRecord,
	finishedResult string,
) {
	t.Helper()
	if len(records) != 2 ||
		records[0].Phase != DeleteAuditStarted ||
		records[1].Phase != DeleteAuditFinished ||
		records[1].Result != finishedResult {
		t.Fatalf("audit records = %#v, want started -> %s", records, finishedResult)
	}
}
