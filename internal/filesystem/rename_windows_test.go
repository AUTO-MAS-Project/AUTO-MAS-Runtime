package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestRenameKind_AuthorizesExactLayoutPairs(t *testing.T) {
	for _, kind := range []RenameKind{
		RenameRepositoryToRetired,
		RenameUpdateToRepository,
		RenameRepositoryRollback,
		RenameUVStagingToVersion,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			operator, _, request := newRenameFixture(t, kind)
			result, err := operator.AtomicRename(t.Context(), request)
			if err != nil || !result.MutationApplied {
				t.Fatalf("AtomicRename() = %#v, %v", result, err)
			}
			if _, err := os.Stat(request.Destination); err != nil {
				t.Fatalf("destination does not exist: %v", err)
			}
			if _, err := os.Stat(request.Source); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("source still exists: %v", err)
			}
		})
	}
}

func TestOperator_AtomicRenameRepositoryRollback(t *testing.T) {
	operator, _, request := newRenameFixture(t, RenameRepositoryRollback)
	marker := filepath.Join(request.Source, "previous-marker")
	if err := os.WriteFile(marker, []byte("previous"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(marker) error = %v", err)
	}

	result, err := operator.AtomicRename(t.Context(), request)
	if err != nil || !result.MutationApplied {
		t.Fatalf("AtomicRename() = %#v, %v, want applied rollback", result, err)
	}
	got, err := os.ReadFile(filepath.Join(request.Destination, "previous-marker"))
	if err != nil || string(got) != "previous" {
		t.Fatalf("rollback marker = %q, %v, want previous", got, err)
	}
	if _, err := os.Lstat(request.Source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback source still exists: %v", err)
	}
}

func TestAtomicRename_RejectsClassifiedSourceIdentityMismatch(t *testing.T) {
	operator, layout, request := newRenameFixture(t, RenameRepositoryRollback)
	inspection, err := InspectManagedDirectory(t.Context(), layout, request.Source)
	if err != nil || !inspection.Exists || inspection.Identity == nil {
		t.Fatalf("InspectManagedDirectory() = %#v, %v, want existing identity", inspection, err)
	}
	expected := *inspection.Identity
	expected.fileID[0]++
	marker := filepath.Join(request.Source, "must-survive.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatalf("WriteFile(marker) error = %v", err)
	}
	request.ExpectedSourceIdentity = &expected
	result, err := operator.AtomicRename(t.Context(), request)
	if result.MutationApplied || !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("AtomicRename() = %#v, %v, want identity rejection without mutation", result, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker after identity rejection: %v", err)
	}
}

func TestAtomicRename_RejectsReplacementAfterClassification(t *testing.T) {
	operator, layout, request := newRenameFixture(t, RenameRepositoryRollback)
	inspection, err := InspectManagedDirectory(t.Context(), layout, request.Source)
	if err != nil || !inspection.Exists || inspection.Identity == nil {
		t.Fatalf("InspectManagedDirectory() = %#v, %v, want existing identity", inspection, err)
	}
	expected := *inspection.Identity
	classifiedMarker := filepath.Join(request.Source, "classified-marker.txt")
	if err := os.WriteFile(classifiedMarker, []byte("classified"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(classified marker) error = %v", err)
	}
	original := request.Source + "-classified"
	if err := os.Rename(request.Source, original); err != nil {
		t.Fatalf("os.Rename(classified source) error = %v", err)
	}
	if err := os.Mkdir(request.Source, 0o700); err != nil {
		t.Fatalf("os.Mkdir(replacement) error = %v", err)
	}
	marker := filepath.Join(request.Source, "replacement-marker.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(marker) error = %v", err)
	}
	request.ExpectedSourceIdentity = &expected

	result, err := operator.AtomicRename(t.Context(), request)
	if result.MutationApplied || !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("AtomicRename() = %#v, %v, want identity rejection without mutation", result, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("replacement marker after identity rejection: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(original, "classified-marker.txt")); err != nil || string(got) != "classified" {
		t.Fatalf("classified marker after identity rejection = %q, %v", got, err)
	}
	if _, err := os.Lstat(request.Destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination after identity rejection: %v", err)
	}
}

func TestAtomicRename_RejectsInvalidFieldsBeforeWin32(t *testing.T) {
	operator, _, valid := newRenameFixture(t, RenameRepositoryToRetired)
	attributeCalls := 0
	attributes := operator.api.attributes
	operator.api.attributes = func(path string) (uint32, error) {
		attributeCalls++
		return attributes(path)
	}
	tests := []struct {
		name   string
		change func(*RenameRequest)
	}{
		{name: "unknown kind", change: func(r *RenameRequest) { r.Kind = "unknown" }},
		{name: "empty source", change: func(r *RenameRequest) { r.Source = "" }},
		{name: "empty destination", change: func(r *RenameRequest) { r.Destination = "" }},
		{name: "empty operation", change: func(r *RenameRequest) { r.OperationID = "" }},
		{name: "multiline reason", change: func(r *RenameRequest) { r.Reason = "a\nb" }},
		{name: "non-uv version", change: func(r *RenameRequest) { r.Version = "0.9.0" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attributeCalls = 0
			request := valid
			test.change(&request)
			result, err := operator.AtomicRename(t.Context(), request)
			if result.MutationApplied || !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("AtomicRename() = %#v, %v", result, err)
			}
			if attributeCalls != 0 {
				t.Fatalf("attribute calls = %d, want 0", attributeCalls)
			}
		})
	}
}

func TestAtomicRename_RejectsCrossVolumeAndReparse(t *testing.T) {
	operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
	request.Destination = `Z:\outside\repo`
	result, err := operator.AtomicRename(t.Context(), request)
	if result.MutationApplied {
		t.Fatal("cross-volume request mutated")
	}
	assertFilesystemCode(t, err, protocol.CodePathOutsideManagedRoot)

	operator, layout, request := newRenameFixture(t, RenameRepositoryToRetired)
	if err := os.Remove(request.Source); err != nil {
		t.Fatalf("os.Remove(source) error = %v", err)
	}
	external := t.TempDir()
	if err := os.Symlink(external, layout.RepoDir()); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	result, err = operator.AtomicRename(t.Context(), request)
	if result.MutationApplied || err == nil {
		t.Fatalf("AtomicRename(reparse) = %#v, %v, want rejection", result, err)
	}
}

func TestAtomicRename_NeverReplacesExistingOrRacingDestination(t *testing.T) {
	t.Run("existing destination", func(t *testing.T) {
		operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
		if err := os.MkdirAll(request.Destination, 0o700); err != nil {
			t.Fatalf("os.MkdirAll(destination) error = %v", err)
		}
		sentinel := filepath.Join(request.Destination, "sentinel")
		if err := os.WriteFile(sentinel, []byte("competitor"), 0o600); err != nil {
			t.Fatalf("os.WriteFile() error = %v", err)
		}
		result, err := operator.AtomicRename(t.Context(), request)
		if result.MutationApplied {
			t.Fatalf("AtomicRename() result = %#v, want not applied", result)
		}
		assertFilesystemCode(t, err, protocol.CodeDirectoryOccupied)
		got, _ := os.ReadFile(sentinel)
		if string(got) != "competitor" {
			t.Fatalf("competitor = %q, want unchanged", got)
		}
	})

	t.Run("destination races final rename", func(t *testing.T) {
		operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
		rename := operator.api.rename
		sentinel := filepath.Join(request.Destination, "sentinel")
		injected := false
		operator.api.rename = func(
			source windows.Handle,
			parent windows.Handle,
			name string,
			replace bool,
		) error {
			injected = true
			if err := os.MkdirAll(request.Destination, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(sentinel, []byte("competitor"), 0o600); err != nil {
				return err
			}
			return rename(source, parent, name, replace)
		}
		result, err := operator.AtomicRename(t.Context(), request)
		if !injected {
			t.Fatal("rename adapter was not called")
		}
		if result.MutationApplied {
			t.Fatalf("AtomicRename() result = %#v, want not applied", result)
		}
		assertFilesystemCode(t, err, protocol.CodeDirectoryOccupied)
		got, readErr := os.ReadFile(sentinel)
		if readErr != nil || string(got) != "competitor" {
			t.Fatalf("competitor = %q, error = %v, want unchanged", got, readErr)
		}
	})
}

func TestAtomicRename_RetriesOnlyTransientErrors(t *testing.T) {
	transient := []error{
		windows.ERROR_SHARING_VIOLATION,
		windows.ERROR_LOCK_VIOLATION,
		windows.ERROR_ACCESS_DENIED,
	}
	for _, injected := range transient {
		t.Run(injected.Error(), func(t *testing.T) {
			operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
			rename := operator.api.rename
			calls := 0
			operator.api.rename = func(
				source windows.Handle,
				parent windows.Handle,
				name string,
				replace bool,
			) error {
				calls++
				if calls == 1 {
					return injected
				}
				return rename(source, parent, name, replace)
			}
			operator.wait = func(context.Context, time.Duration) error { return nil }
			result, err := operator.AtomicRename(t.Context(), request)
			if err != nil || !result.MutationApplied || calls != 2 {
				t.Fatalf("AtomicRename() = %#v, %v, calls=%d", result, err, calls)
			}
		})
	}
}

func TestAtomicRename_RetriesPinSharingViolationBeforeRename(t *testing.T) {
	operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
	operator.delays = []time.Duration{10 * time.Millisecond}
	openRelative := operator.api.openRelative
	pinCalls := 0
	operator.api.openRelative = func(
		parent windows.Handle,
		name string,
		spec openSpec,
	) (windows.Handle, error) {
		if name == filepath.Base(request.Source) && spec == renameSourceSpec() {
			pinCalls++
			if pinCalls == 1 {
				return windows.InvalidHandle, windows.ERROR_SHARING_VIOLATION
			}
		}
		return openRelative(parent, name, spec)
	}
	var waits []time.Duration
	operator.wait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}

	result, err := operator.AtomicRename(t.Context(), request)
	if err != nil || !result.MutationApplied {
		t.Fatalf("AtomicRename() = %#v, %v, want retry success", result, err)
	}
	if pinCalls != 2 || len(waits) != 1 || waits[0] != operator.delays[0] {
		t.Fatalf("pin calls/waits = %d/%v, want 2/%v", pinCalls, waits, operator.delays)
	}
}

func TestAtomicRename_DoesNotRetryParentAccessDeniedAsOccupied(t *testing.T) {
	operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
	openRelative := operator.api.openRelative
	injected := false
	operator.api.openRelative = func(
		parent windows.Handle,
		name string,
		spec openSpec,
	) (windows.Handle, error) {
		if !injected && spec == directoryPinSpec() {
			injected = true
			return windows.InvalidHandle, windows.ERROR_ACCESS_DENIED
		}
		return openRelative(parent, name, spec)
	}
	waitCalls := 0
	operator.wait = func(context.Context, time.Duration) error {
		waitCalls++
		return nil
	}

	result, err := operator.AtomicRename(t.Context(), request)
	if result.MutationApplied {
		t.Fatalf("AtomicRename() result = %#v, want no mutation", result)
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("AtomicRename() error = %v, want access denied", err)
	}
	if waitCalls != 0 {
		t.Fatalf("wait calls = %d, want 0 for non-leaf access failure", waitCalls)
	}
}

func TestTransientRenamePinError_RejectsCloseFailure(t *testing.T) {
	source := `C:\managed\repo`
	openErr := &FileError{
		Operation: "open-relative",
		Path:      source,
		Err:       windows.ERROR_ACCESS_DENIED,
	}
	closeErr := &FileError{
		Operation: "close",
		Path:      `C:\managed`,
		Err:       windows.ERROR_ACCESS_DENIED,
	}
	if isTransientRenamePinError(openErr, source) != true {
		t.Fatal("source leaf access failure was not classified as transient")
	}
	if isTransientRenamePinError(errors.Join(openErr, closeErr), source) {
		t.Fatal("pin close failure was classified as transient")
	}
}

func TestAtomicRename_MapsPinSharingViolationAfterRetryExhaustion(t *testing.T) {
	operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
	operator.delays = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	openRelative := operator.api.openRelative
	pinCalls := 0
	operator.api.openRelative = func(
		parent windows.Handle,
		name string,
		spec openSpec,
	) (windows.Handle, error) {
		if name == filepath.Base(request.Source) && spec == renameSourceSpec() {
			pinCalls++
			return windows.InvalidHandle, windows.ERROR_LOCK_VIOLATION
		}
		return openRelative(parent, name, spec)
	}
	var waits []time.Duration
	operator.wait = func(_ context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return nil
	}

	result, err := operator.AtomicRename(t.Context(), request)
	if result.MutationApplied {
		t.Fatalf("AtomicRename() = %#v, want pre-mutation occupied", result)
	}
	assertFilesystemCode(t, err, protocol.CodeDirectoryOccupied)
	if pinCalls != len(operator.delays)+1 || len(waits) != len(operator.delays) {
		t.Fatalf("pin calls/waits = %d/%v, want %d/%v", pinCalls, waits, len(operator.delays)+1, operator.delays)
	}
}

func TestAtomicRename_UsesConfiguredDelaySequence(t *testing.T) {
	operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
	operator.delays = []time.Duration{
		50 * time.Millisecond,
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
	}
	var got []time.Duration
	operator.wait = func(_ context.Context, delay time.Duration) error {
		got = append(got, delay)
		return nil
	}
	operator.api.rename = func(
		windows.Handle,
		windows.Handle,
		string,
		bool,
	) error {
		return windows.ERROR_SHARING_VIOLATION
	}
	result, err := operator.AtomicRename(t.Context(), request)
	if result.MutationApplied {
		t.Fatalf("AtomicRename() result = %#v, want not applied", result)
	}
	assertFilesystemCode(t, err, protocol.CodeDirectoryOccupied)
	want := operator.delays
	if len(got) != len(want) {
		t.Fatalf("wait delays = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("wait delay[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestAtomicRename_RevalidatesIdentityBeforeEveryRetry(t *testing.T) {
	operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
	attributeCalls := 0
	attributes := operator.api.attributes
	operator.api.attributes = func(path string) (uint32, error) {
		attributeCalls++
		return attributes(path)
	}
	rename := operator.api.rename
	calls := 0
	operator.api.rename = func(
		source windows.Handle,
		parent windows.Handle,
		name string,
		replace bool,
	) error {
		calls++
		if calls == 1 {
			return windows.ERROR_SHARING_VIOLATION
		}
		return rename(source, parent, name, replace)
	}
	operator.wait = func(context.Context, time.Duration) error { return nil }
	if _, err := operator.AtomicRename(t.Context(), request); err != nil {
		t.Fatalf("AtomicRename() error = %v", err)
	}
	if calls != 2 || attributeCalls < 4 {
		t.Fatalf("rename/attribute calls = %d/%d, want 2/revalidated", calls, attributeCalls)
	}
}

func TestAtomicRename_ContextCancelsWaitAndStopsRetries(t *testing.T) {
	operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
	ctx, cancel := context.WithCancel(t.Context())
	operator.api.rename = func(
		windows.Handle,
		windows.Handle,
		string,
		bool,
	) error {
		return windows.ERROR_SHARING_VIOLATION
	}
	operator.wait = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	result, err := operator.AtomicRename(ctx, request)
	if result.MutationApplied || !errors.Is(err, context.Canceled) {
		t.Fatalf("AtomicRename() = %#v, %v", result, err)
	}
}

func TestAtomicRename_CancellationAfterPinStopsBeforeMutation(t *testing.T) {
	operator, layout, request := newRenameFixture(t, RenameRepositoryToRetired)
	ctx, cancel := context.WithCancel(t.Context())
	caseSensitive := operator.api.caseSensitive
	finalPath := operator.api.finalPath
	parentChecks := 0
	operator.api.caseSensitive = func(handle windows.Handle) (bool, error) {
		value, err := caseSensitive(handle)
		if err != nil {
			return value, err
		}
		path, pathErr := finalPath(handle)
		if pathErr == nil {
			display, displayErr := displayWindowsPath(path)
			if displayErr == nil && sameRenamePath(display, layout.AppRoot()) {
				parentChecks++
				if parentChecks == 2 {
					cancel()
				}
			}
		}
		return value, nil
	}
	renameCalls := 0
	rename := operator.api.rename
	operator.api.rename = func(
		source windows.Handle,
		parent windows.Handle,
		name string,
		replace bool,
	) error {
		renameCalls++
		return rename(source, parent, name, replace)
	}

	result, err := operator.AtomicRename(ctx, request)
	if result.MutationApplied || !errors.Is(err, context.Canceled) {
		t.Fatalf("AtomicRename() = %#v, %v, want pre-mutation cancellation", result, err)
	}
	if renameCalls != 0 {
		t.Fatalf("rename calls = %d, want 0 after cancellation", renameCalls)
	}
	if _, statErr := os.Stat(request.Source); statErr != nil {
		t.Fatalf("source after cancellation: %v", statErr)
	}
}

func TestAtomicRename_MapsCrossDeviceWithoutRetry(t *testing.T) {
	operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
	waitCalls := 0
	operator.wait = func(context.Context, time.Duration) error {
		waitCalls++
		return nil
	}
	operator.api.rename = func(
		windows.Handle,
		windows.Handle,
		string,
		bool,
	) error {
		return windows.ERROR_NOT_SAME_DEVICE
	}
	result, err := operator.AtomicRename(t.Context(), request)
	if result.MutationApplied {
		t.Fatalf("AtomicRename() result = %#v, want not applied", result)
	}
	assertFilesystemCode(t, err, protocol.CodePathOutsideManagedRoot)
	if waitCalls != 0 {
		t.Fatalf("wait calls = %d, want 0", waitCalls)
	}
}

func TestAtomicRename_MapsOccupiedErrors(t *testing.T) {
	operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
	closeInjected := errors.New("occupied close failed")
	closeHandle := operator.api.closeHandle
	renameAttempted := false
	operator.api.closeHandle = func(handle windows.Handle) error {
		err := closeHandle(handle)
		if renameAttempted {
			renameAttempted = false
			return errors.Join(closeInjected, err)
		}
		return err
	}
	operator.api.rename = func(
		windows.Handle,
		windows.Handle,
		string,
		bool,
	) error {
		renameAttempted = true
		return windows.ERROR_ALREADY_EXISTS
	}
	result, err := operator.AtomicRename(t.Context(), request)
	if result.MutationApplied {
		t.Fatalf("AtomicRename() result = %#v, want not applied", result)
	}
	assertFilesystemCode(t, err, protocol.CodeDirectoryOccupied)
	if !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("AtomicRename() error = %v, want ErrDestinationExists", err)
	}
	if !errors.Is(err, closeInjected) {
		t.Fatalf("AtomicRename() error = %v, want close chain", err)
	}
}

func TestAtomicRename_ReportsAppliedBeforeCloseError(t *testing.T) {
	operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
	injected := errors.New("close failed")
	closeHandle := operator.api.closeHandle
	rename := operator.api.rename
	renamed := false
	operator.api.rename = func(
		source windows.Handle,
		parent windows.Handle,
		name string,
		replace bool,
	) error {
		err := rename(source, parent, name, replace)
		renamed = err == nil
		return err
	}
	operator.api.closeHandle = func(handle windows.Handle) error {
		err := closeHandle(handle)
		if renamed {
			renamed = false
			return errors.Join(injected, err)
		}
		return err
	}
	result, err := operator.AtomicRename(t.Context(), request)
	if !result.MutationApplied || !errors.Is(err, injected) {
		t.Fatalf("AtomicRename() = %#v, %v, want applied/injected", result, err)
	}
	if _, statErr := os.Stat(request.Destination); statErr != nil {
		t.Fatalf("destination missing after applied close error: %v", statErr)
	}
}

func TestAtomicRename_PinsSourceAndDestinationParent(t *testing.T) {
	testAtomicRenamePinsSourceAndDestinationParent(t)
}

func testAtomicRenamePinsSourceAndDestinationParent(t *testing.T) {
	t.Helper()
	operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
	rename := operator.api.rename
	entered := make(chan struct{})
	release := make(chan struct{})
	operator.api.rename = func(
		source windows.Handle,
		parent windows.Handle,
		name string,
		replace bool,
	) error {
		close(entered)
		<-release
		return rename(source, parent, name, replace)
	}
	done := make(chan error, 1)
	go func() {
		_, err := operator.AtomicRename(t.Context(), request)
		done <- err
	}()
	<-entered
	if err := os.Rename(request.Source, request.Source+"-other"); err == nil {
		close(release)
		t.Fatal("source renamed while AtomicRename held its handle")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("AtomicRename() error = %v", err)
	}
}

func TestAtomicRename_ContextsRejectedBeforeIO(t *testing.T) {
	operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
	attributeCalls := 0
	attributes := operator.api.attributes
	operator.api.attributes = func(path string) (uint32, error) {
		attributeCalls++
		return attributes(path)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := operator.AtomicRename(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("AtomicRename() error = %v", err)
	}
	if _, err := operator.AtomicRename(nil, request); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("AtomicRename(nil) error = %v", err)
	}
	if attributeCalls != 0 {
		t.Fatalf("attribute calls = %d, want 0", attributeCalls)
	}
}

func TestAtomicRename_CancellationAfterRenameKeepsApplied(t *testing.T) {
	operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
	ctx, cancel := context.WithCancel(t.Context())
	rename := operator.api.rename
	operator.api.rename = func(
		source windows.Handle,
		parent windows.Handle,
		name string,
		replace bool,
	) error {
		err := rename(source, parent, name, replace)
		if err == nil {
			cancel()
		}
		return err
	}
	result, err := operator.AtomicRename(ctx, request)
	if err != nil || !result.MutationApplied {
		t.Fatalf("AtomicRename() = %#v, %v, want applied/nil", result, err)
	}
}

func TestAtomicRename_MatchesAccessMatrixAndParentIdentity(t *testing.T) {
	operator, _, request := newRenameFixture(t, RenameRepositoryToRetired)
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
	if _, err := operator.AtomicRename(t.Context(), request); err != nil {
		t.Fatalf("AtomicRename() error = %v", err)
	}
	if !containsOpenSpec(specs, renameSourceSpec()) ||
		!containsOpenSpec(specs, directoryPinSpec()) {
		t.Fatalf("open specs = %#v, want source and parent specs", specs)
	}
}

func TestWindows_AtomicRenamePinsSourceAndDestinationAcrossRace(t *testing.T) {
	testAtomicRenamePinsSourceAndDestinationParent(t)
}

func newRenameFixture(
	t *testing.T,
	kind RenameKind,
) (*Operator, *config.Layout, RenameRequest) {
	t.Helper()
	layout := newDeleteTestLayout(t)
	request := RenameRequest{
		Kind:        kind,
		OperationID: "operation-a",
		Reason:      "test rename",
	}
	switch kind {
	case RenameRepositoryToRetired:
		request.Source = layout.RepoDir()
		request.Destination, _ = layout.RepoPreviousDir(request.OperationID)
	case RenameUpdateToRepository:
		request.Source, _ = layout.RepoUpdateDir(request.OperationID)
		request.Destination = layout.RepoDir()
	case RenameRepositoryRollback:
		request.Source, _ = layout.RepoPreviousDir(request.OperationID)
		request.Destination = layout.RepoDir()
	case RenameUVStagingToVersion:
		request.Version = "0.9.0"
		request.Source, _ = layout.UVStagingDir(request.Version, request.OperationID)
		request.Destination, _ = layout.UVVersionDir(request.Version)
	default:
		t.Fatalf("unsupported test rename kind %q", kind)
	}
	if err := os.MkdirAll(request.Source, 0o700); err != nil {
		t.Fatalf("os.MkdirAll(source) error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(request.Destination), 0o700); err != nil {
		t.Fatalf("os.MkdirAll(destination parent) error = %v", err)
	}
	operator, err := New(t.Context(), layout, &recordingDeleteAuditor{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return operator, layout, request
}
