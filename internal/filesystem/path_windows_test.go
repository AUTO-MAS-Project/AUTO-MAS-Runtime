package filesystem

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestCanonicalize_RejectsUnsafeSyntaxBeforeIO(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
	}{
		{name: "empty", path: ""},
		{name: "nul", path: "C:\\AUTO-MAS\x00\\repo"},
		{name: "drive relative", path: `C:repo`},
		{name: "dot", path: `.`},
		{name: "parent relative", path: `..\repo`},
		{name: "physical device", path: `\\.\PhysicalDrive0`},
		{name: "global root", path: `\\?\GLOBALROOT\Device`},
		{name: "reserved tail", path: `C:\AUTO-MAS\CON`},
		{name: "alternate stream", path: `C:\AUTO-MAS\name:stream`},
		{name: "empty component", path: `C:\AUTO-MAS\\repo`},
		{name: "trailing dot", path: `C:\AUTO-MAS\repo.`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			api := newProductionPathAPI()
			api.attributes = func(string) (uint32, error) {
				t.Fatal("attributes called for invalid syntax")
				return 0, nil
			}
			api.openPath = func(string, openSpec) (windows.Handle, error) {
				t.Fatal("openPath called for invalid syntax")
				return windows.InvalidHandle, nil
			}
			got, err := canonicalizeContextWith(t.Context(), test.path, api)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("canonicalizeContextWith(%q) error = %v, want ErrInvalidArgument", test.path, err)
			}
			if got != (CanonicalPath{}) {
				t.Fatalf("canonicalizeContextWith(%q) = %#v, want zero value", test.path, got)
			}
		})
	}
}

func TestCanonicalize_ContextRejectedBeforeIO(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		want error
	}{
		{name: "nil", ctx: nil, want: ErrInvalidArgument},
		{
			name: "pre-canceled",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			}(),
			want: context.Canceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newProductionPathAPI()
			calls := 0
			api.attributes = func(string) (uint32, error) {
				calls++
				return 0, errors.New("unexpected attributes")
			}
			api.openPath = func(string, openSpec) (windows.Handle, error) {
				calls++
				return windows.InvalidHandle, errors.New("unexpected open")
			}
			if _, err := canonicalizeContextWith(
				test.ctx,
				`C:\AUTO-MAS`,
				api,
			); !errors.Is(err, test.want) {
				t.Fatalf("canonicalizeContextWith() error = %v, want %v", err, test.want)
			}
			if calls != 0 {
				t.Fatalf("I/O calls = %d, want 0", calls)
			}
		})
	}
}

func TestCanonicalize_NormalizesEquivalentForms(t *testing.T) {
	root := t.TempDir()
	mixed := filepath.Join(root, "MiXeD")
	if err := os.Mkdir(mixed, 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}

	canonical := mustCanonicalize(t, mixed)
	forms := []string{
		mixed + `\`,
		filepath.Join(root, "mixed"),
		extendedPathForTest(mixed),
	}
	for _, form := range forms {
		got := mustCanonicalize(t, form)
		if !canonical.Equal(got) {
			t.Errorf("Canonicalize(%q).Equal(canonical) = false, want true", form)
		}
	}
	if !filepath.IsAbs(canonical.String()) {
		t.Fatalf("String() = %q, want absolute path", canonical.String())
	}
	if !strings.HasPrefix(canonical.Native(), `\\?\`) {
		t.Fatalf("Native() = %q, want extended prefix", canonical.Native())
	}
}

func TestCanonicalize_ExpandsExistingShortPath(t *testing.T) {
	root := t.TempDir()
	longName := filepath.Join(root, "Directory With A Long Name")
	if err := os.Mkdir(longName, 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	shortName, ok := shortPathForTest(t, longName)
	if !ok {
		t.Skip("volume does not expose an 8.3 short path for the fixture")
	}

	longCanonical := mustCanonicalize(t, longName)
	shortCanonical := mustCanonicalize(t, shortName)
	if !longCanonical.Equal(shortCanonical) {
		t.Fatalf("long.Equal(short) = false, want true")
	}
}

func TestCanonicalize_AllowsOnlySafeNonexistentTail(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "future", "leaf")
	got := mustCanonicalize(t, target)
	parent := mustCanonicalize(t, root)
	if !parent.Contains(got) {
		t.Fatalf("Canonicalize(root).Contains(target) = false, want true")
	}
	if got.String() != target {
		t.Fatalf("String() = %q, want %q", got.String(), target)
	}
}

func TestCanonicalPath_ContainsUsesVolumeAndComponentBoundary(t *testing.T) {
	t.Parallel()

	parent := canonicalPathForTest(`C:\AUTO-MAS`)
	tests := []struct {
		name  string
		child CanonicalPath
		want  bool
	}{
		{name: "same path", child: canonicalPathForTest(`c:\auto-mas`), want: false},
		{name: "direct child", child: canonicalPathForTest(`c:\auto-mas\repo`), want: true},
		{name: "deep child", child: canonicalPathForTest(`C:\AUTO-MAS\repo\src`), want: true},
		{name: "sibling prefix", child: canonicalPathForTest(`C:\AUTO-MAS-2`), want: false},
		{name: "component prefix", child: canonicalPathForTest(`C:\AUTO-MAS-old\repo`), want: false},
		{name: "other volume", child: canonicalPathForTest(`D:\AUTO-MAS\repo`), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := parent.Contains(test.child); got != test.want {
				t.Fatalf("Contains(%q) = %t, want %t", test.child.String(), got, test.want)
			}
		})
	}
}

func TestCanonicalPath_UsesOrdinalNonASCIIEquivalence(t *testing.T) {
	t.Parallel()

	upper := canonicalPathForTest(`C:\AUTO-MAS\Äpfel`)
	lower := canonicalPathForTest(`c:\auto-mas\äPFEL`)
	if !upper.Equal(lower) {
		t.Fatal("ordinal case variants are not equal")
	}
	parent := canonicalPathForTest(`C:\AUTO-MAS\Äpfel`)
	child := canonicalPathForTest(`c:\auto-mas\äPFEL\cache`)
	if !parent.Contains(child) {
		t.Fatal("ordinal case variant child is not contained")
	}

	composed := canonicalPathForTest("C:\\AUTO-MAS\\\u00e9")
	decomposed := canonicalPathForTest("C:\\AUTO-MAS\\e\u0301")
	if composed.Equal(decomposed) {
		t.Fatal("composed and decomposed forms are ordinal-equal, want distinct")
	}
}

func TestCanonicalPath_OrdinalComparisonFailureFailsClosed(t *testing.T) {
	t.Parallel()

	valid := canonicalPathForTest(`C:\AUTO-MAS\repo`)
	badVolume := valid
	badVolume.volumeKey = "C:\x00"
	if badVolume.Equal(valid) || badVolume.Contains(valid) {
		t.Fatal("invalid volume comparison authorized a path")
	}

	badPath := valid
	badPath.comparisonKey = "C:\\AUTO-MAS\x00\\repo"
	descendant := canonicalPathForTest(`C:\AUTO-MAS\repo\child`)
	if badPath.Equal(valid) || badPath.Contains(descendant) {
		t.Fatal("invalid full-path comparison authorized a path")
	}
}

func TestCanonicalize_ExistingPrefixMatchesAccessMatrix(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "state.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	tests := []struct {
		name      string
		path      string
		directory bool
	}{
		{name: "directory", path: root, directory: true},
		{name: "file", path: file, directory: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newProductionPathAPI()
			openPath := api.openPath
			closeHandle := api.closeHandle
			var got openSpec
			closeCalls := 0
			api.openPath = func(path string, spec openSpec) (windows.Handle, error) {
				got = spec
				return openPath(path, spec)
			}
			api.closeHandle = func(handle windows.Handle) error {
				closeCalls++
				return closeHandle(handle)
			}

			if _, err := canonicalizeContextWith(t.Context(), test.path, api); err != nil {
				t.Fatalf("canonicalizeContextWith() error = %v", err)
			}
			want := openSpec{
				access:    windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
				share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE,
				creation:  windows.OPEN_EXISTING,
				options:   windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT,
				directory: test.directory,
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("openSpec = %#v, want %#v", got, want)
			}
			if closeCalls != 1 {
				t.Fatalf("closeHandle calls = %d, want 1", closeCalls)
			}
		})
	}
}

func TestFilesystemEnums_StringAndValid(t *testing.T) {
	t.Parallel()

	type enum interface {
		String() string
		Valid() bool
	}
	tests := []struct {
		name  string
		value enum
		want  string
	}{
		{name: "state backend", value: StateBackend, want: "backend"},
		{name: "state mutation", value: StateMutation, want: "mutation"},
		{name: "state update", value: StateUpdate, want: "update"},
		{name: "state environment", value: StateEnvironment, want: "environment"},
		{name: "state write recover", value: StateWritePhaseRecover, want: "recover"},
		{name: "state write create", value: StateWritePhaseCreate, want: "create"},
		{name: "state write write", value: StateWritePhaseWrite, want: "write"},
		{name: "state write sync", value: StateWritePhaseSync, want: "sync"},
		{name: "state write rename", value: StateWritePhaseRename, want: "rename"},
		{name: "state write finalize", value: StateWritePhaseFinalize, want: "finalize"},
		{name: "state write close", value: StateWritePhaseClose, want: "close"},
		{name: "delete uv cache", value: DeleteUVCache, want: "uv_cache"},
		{name: "delete managed venv", value: DeleteManagedVenv, want: "managed_venv"},
		{name: "delete managed python", value: DeleteManagedPython, want: "managed_python"},
		{name: "delete repo update", value: DeleteRepositoryUpdate, want: "repository_update"},
		{name: "delete repo retired", value: DeleteRepositoryRetired, want: "repository_retired"},
		{name: "delete temporary", value: DeleteDownloadTemporary, want: "download_temporary"},
		{name: "delete uv staging", value: DeleteUVStaging, want: "uv_staging"},
		{name: "delete pycache", value: DeletePythonCache, want: "python_cache"},
		{name: "delete build cache", value: DeleteBuildCache, want: "build_cache"},
		{name: "audit started", value: DeleteAuditStarted, want: "started"},
		{name: "audit finished", value: DeleteAuditFinished, want: "finished"},
		{name: "rename retired", value: RenameRepositoryToRetired, want: "repository_to_retired"},
		{name: "rename repository", value: RenameUpdateToRepository, want: "update_to_repository"},
		{name: "rename uv", value: RenameUVStagingToVersion, want: "uv_staging_to_version"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !test.value.Valid() {
				t.Fatal("Valid() = false, want true")
			}
			if got := test.value.String(); got != test.want {
				t.Fatalf("String() = %q, want %q", got, test.want)
			}
		})
	}

	invalid := []enum{
		StateFileKind(""),
		StateFileKind("unknown"),
		StateWritePhase(""),
		StateWritePhase("unknown"),
		DeleteKind(""),
		DeleteKind("unknown"),
		DeleteAuditPhase(""),
		DeleteAuditPhase("unknown"),
		RenameKind(""),
		RenameKind("unknown"),
	}
	for _, value := range invalid {
		if value.Valid() {
			t.Errorf("%T(%q).Valid() = true, want false", value, value.String())
		}
	}
}

func TestFilesystemErrors_PreserveChainsAndCodes(t *testing.T) {
	t.Parallel()

	cause := errors.New("injected")
	pathErr := &Error{
		code:      protocol.CodePathOutsideManagedRoot,
		Operation: "authorize",
		Path:      `C:\AUTO-MAS\repo`,
		Err:       cause,
	}
	if !errors.Is(pathErr, cause) {
		t.Fatalf("errors.Is(%v, cause) = false, want true", pathErr)
	}
	if got := pathErr.Code(); got != protocol.CodePathOutsideManagedRoot {
		t.Fatalf("Code() = %q, want %q", got, protocol.CodePathOutsideManagedRoot)
	}

	fileErr := &FileError{Operation: "open", Path: pathErr.Path, Err: cause}
	if !errors.Is(fileErr, cause) {
		t.Fatalf("errors.Is(%v, cause) = false, want true", fileErr)
	}
	tooLarge := &FileError{
		Operation: "size-limit",
		Path:      pathErr.Path,
		Err:       ErrStateFileTooLarge,
	}
	if !errors.Is(tooLarge, ErrStateFileTooLarge) {
		t.Fatalf("errors.Is(%v, ErrStateFileTooLarge) = false, want true", tooLarge)
	}
	auditErr := &AuditError{
		Phase:           DeleteAuditFinished,
		MutationApplied: true,
		Cause:           cause,
	}
	if !errors.Is(auditErr, cause) {
		t.Fatalf("errors.Is(%v, cause) = false, want true", auditErr)
	}

	cleanup := errors.New("cleanup failed")
	writeErr := &StateWriteError{
		Phase:            StateWritePhaseFinalize,
		MutationApplied:  true,
		RecoveryRequired: true,
		Cause:            fileErr,
		CleanupError:     cleanup,
	}
	if !errors.Is(writeErr, cause) || !errors.Is(writeErr, cleanup) {
		t.Fatalf("StateWriteError causes = %v, want primary and cleanup", writeErr)
	}
	if writeErr.Phase != StateWritePhaseFinalize ||
		!writeErr.MutationApplied ||
		!writeErr.RecoveryRequired {
		t.Fatalf("StateWriteError = %#v, want finalize/applied/recovery", writeErr)
	}

	removeErr := &StateRemoveError{Cause: ErrIdentityChanged, CleanupError: cleanup}
	if !errors.Is(removeErr, ErrIdentityChanged) || !errors.Is(removeErr, cleanup) {
		t.Fatalf("StateRemoveError causes = %v, want identity and cleanup", removeErr)
	}
	var typedRemove *StateRemoveError
	if !errors.As(removeErr, &typedRemove) ||
		typedRemove.Cause != ErrIdentityChanged ||
		typedRemove.CleanupError != cleanup {
		t.Fatalf("StateRemoveError = %#v, want public cause fields", typedRemove)
	}
	if got := removeErr.Error(); got == "" ||
		strings.Contains(got, cleanup.Error()) {
		t.Fatalf("StateRemoveError.Error() = %q, want stable text without cause payload", got)
	}
	if errors.Is(fs.ErrNotExist, ErrStateFileNotFound) {
		t.Fatal("raw fs.ErrNotExist matches ErrStateFileNotFound")
	}
	for _, sentinel := range []error{
		ErrStateFileNotFound,
		ErrStateRecoveryRequired,
	} {
		if sentinel == nil || sentinel.Error() == "" {
			t.Fatalf("sentinel = %v, want stable non-empty error", sentinel)
		}
	}
	if got := ErrPOSIXUnlinkUnsupported.Error(); got !=
		"filesystem POSIX unlink is unsupported" {
		t.Fatalf("ErrPOSIXUnlinkUnsupported.Error() = %q", got)
	}
}

func TestWindows_CanonicalPathLongAndShortFormsCannotBypassProtection(t *testing.T) {
	root := t.TempDir()
	longPath := root
	for i := 0; i < 12; i++ {
		longPath = filepath.Join(longPath, strings.Repeat("segment", 4))
	}
	if err := os.MkdirAll(extendedPathForTest(longPath), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	longCanonical := mustCanonicalize(t, longPath)
	extendedCanonical := mustCanonicalize(t, extendedPathForTest(longPath))
	if !longCanonical.Equal(extendedCanonical) {
		t.Fatal("long and extended forms are not equal")
	}

	sibling := mustCanonicalize(t, longPath+"-other")
	if longCanonical.Contains(sibling) {
		t.Fatal("sibling prefix was accepted as a descendant")
	}
	if shortPath, ok := shortPathForTest(t, longPath); ok {
		shortCanonical := mustCanonicalize(t, shortPath)
		if !longCanonical.Equal(shortCanonical) {
			t.Fatal("long and 8.3 forms are not equal")
		}
	}
}

func mustCanonicalize(t *testing.T, path string) CanonicalPath {
	t.Helper()
	got, err := Canonicalize(path)
	if err != nil {
		t.Fatalf("Canonicalize(%q) error = %v", path, err)
	}
	return got
}

func canonicalPathForTest(path string) CanonicalPath {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	return CanonicalPath{
		display:       clean,
		native:        extendedPathForTest(clean),
		comparisonKey: clean,
		volumeKey:     volume,
	}
}

func extendedPathForTest(path string) string {
	if strings.HasPrefix(path, `\\?\`) {
		return path
	}
	if strings.HasPrefix(path, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(path, `\\`)
	}
	return `\\?\` + path
}

func shortPathForTest(t *testing.T, path string) (string, bool) {
	t.Helper()

	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("windows.UTF16PtrFromString() error = %v", err)
	}
	buffer := make([]uint16, 32768)
	procedure := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetShortPathNameW")
	length, _, callErr := procedure.Call(
		uintptr(unsafe.Pointer(pathUTF16)),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
	)
	if length == 0 {
		if errors.Is(callErr, windows.ERROR_SUCCESS) {
			t.Fatalf("GetShortPathNameW(%q) returned no path", path)
		}
		return "", false
	}
	if length >= uintptr(len(buffer)) {
		t.Fatalf("GetShortPathNameW length = %d, buffer = %d", length, len(buffer))
	}
	short := windows.UTF16ToString(buffer[:length])
	if short == path || !strings.Contains(short, "~") {
		return "", false
	}
	return short, true
}

func TestPinnedChain_RejectsReparsePointAtEveryLevel(t *testing.T) {
	parent := t.TempDir()
	external := t.TempDir()
	link := filepath.Join(parent, "linked")
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	rootParent := mustCanonicalize(t, parent)
	target := mustCanonicalize(t, link)
	chain, err := openPinnedChainWith(
		t.Context(),
		rootParent,
		target,
		directoryPinSpec(),
		newProductionPathAPI(),
	)
	if chain != nil {
		_ = chain.close()
		t.Fatal("openPinnedChainWith() returned a chain through a reparse point")
	}
	assertFilesystemCode(t, err, protocol.CodeUnsafeReparsePoint)
}

func TestPinnedChain_RejectsIdentityOrParentMismatch(t *testing.T) {
	chain := openPinnedTestChain(t)
	defer func() {
		if err := chain.close(); err != nil {
			t.Errorf("chain.close() error = %v", err)
		}
	}()
	parent := chain.objects[len(chain.objects)-2]
	child := chain.objects[len(chain.objects)-1]
	parent.identity.fileID[0] ^= 0xff
	err := validateParentIdentityWith(t.Context(), parent, child, chain.api)
	if !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("validateParentIdentityWith() error = %v, want ErrIdentityChanged", err)
	}
}

func TestPinnedChain_HoldsEveryAncestorUntilClose(t *testing.T) {
	chain := openPinnedTestChain(t)
	target := chain.objects[len(chain.objects)-1].path.String()
	replacement := target + "-renamed"
	if err := os.Rename(target, replacement); err == nil {
		_ = os.Rename(replacement, target)
		_ = chain.close()
		t.Fatal("os.Rename() succeeded while the target was pinned")
	}
	if err := chain.close(); err != nil {
		t.Fatalf("chain.close() error = %v", err)
	}
	if err := os.Rename(target, replacement); err != nil {
		t.Fatalf("os.Rename() after close error = %v", err)
	}
}

func TestDangerousRootValidation_RejectsUNCAndRemoteRoots(t *testing.T) {
	api := newProductionPathAPI()
	api.driveType = func(string) (uint32, error) {
		return windows.DRIVE_REMOTE, nil
	}
	path := mustCanonicalize(t, t.TempDir())
	err := ensureLocalVolumeWith(path, api)
	if err == nil {
		t.Fatal("ensureLocalVolumeWith() error = nil, want remote-volume rejection")
	}

	unc := canonicalPathForTest(`\\server\share\AUTO-MAS`)
	if err := ensureLocalVolumeWith(unc, api); err == nil {
		t.Fatal("ensureLocalVolumeWith(UNC) error = nil, want rejection")
	}
}

func TestDangerousRootValidation_RejectsCaseSensitiveAncestor(t *testing.T) {
	api := newProductionPathAPI()
	api.caseSensitive = func(windows.Handle) (bool, error) { return true, nil }
	parent := t.TempDir()
	target := filepath.Join(parent, "app")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	chain, err := openPinnedChainWith(
		t.Context(),
		mustCanonicalize(t, parent),
		mustCanonicalize(t, target),
		directoryPinSpec(),
		api,
	)
	if chain != nil {
		_ = chain.close()
		t.Fatal("openPinnedChainWith() returned a case-sensitive chain")
	}
	if !errors.Is(err, ErrUnsupportedCaseSensitivity) {
		t.Fatalf("error = %v, want ErrUnsupportedCaseSensitivity", err)
	}
}

func TestDangerousRootValidation_RejectsIndeterminateCaseSensitivity(t *testing.T) {
	injected := errors.New("case query failed")
	api := newProductionPathAPI()
	api.caseSensitive = func(windows.Handle) (bool, error) {
		return false, injected
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "app")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	chain, err := openPinnedChainWith(
		t.Context(),
		mustCanonicalize(t, parent),
		mustCanonicalize(t, target),
		directoryPinSpec(),
		api,
	)
	if chain != nil {
		_ = chain.close()
		t.Fatal("openPinnedChainWith() returned an indeterminate chain")
	}
	if !errors.Is(err, injected) ||
		!errors.Is(err, ErrUnsupportedCaseSensitivity) {
		t.Fatalf("error = %v, want injected and unsupported-case errors", err)
	}
}

func TestDangerousRootValidation_RejectsNilContextAndLayout(t *testing.T) {
	layout := newPathTestLayout(t)
	api := newProductionPathAPI()
	tests := []struct {
		name   string
		ctx    context.Context
		layout *config.Layout
	}{
		{name: "nil context", layout: layout},
		{name: "nil layout", ctx: t.Context()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			chains, err := validateDangerousRootWith(
				test.ctx,
				test.layout,
				[]string{layout.AppRoot()},
				api,
			)
			if chains != nil {
				t.Fatalf("chains = %#v, want nil", chains)
			}
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestOpenSpec_DirectoryPinMatchesMatrix(t *testing.T) {
	got := directoryPinSpec()
	want := openSpec{
		access:    windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: true,
	}
	if got != want {
		t.Fatalf("directoryPinSpec() = %#v, want %#v", got, want)
	}
}

func TestOpenSpec_ParentIdentityMatchesMatrix(t *testing.T) {
	got := parentIdentitySpec()
	want := openSpec{
		access:    windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: true,
	}
	if got != want {
		t.Fatalf("parentIdentitySpec() = %#v, want %#v", got, want)
	}
}

func TestOpenRelativeChecked_ValidatesParentIdentity(t *testing.T) {
	chain := openPinnedTestChain(t)
	parent := chain.objects[len(chain.objects)-2]
	child := chain.objects[len(chain.objects)-1]
	if err := validateParentIdentityWith(t.Context(), parent, child, chain.api); err != nil {
		_ = chain.close()
		t.Fatalf("validateParentIdentityWith() error = %v", err)
	}
	if err := chain.close(); err != nil {
		t.Fatalf("chain.close() error = %v", err)
	}
}

func TestWindows_ReparsePointsNeverEscapeManagedRoot(t *testing.T) {
	parent := t.TempDir()
	external := t.TempDir()
	sentinel := filepath.Join(external, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	link := filepath.Join(parent, "escape")
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	chain, err := openPinnedChainWith(
		t.Context(),
		mustCanonicalize(t, parent),
		mustCanonicalize(t, link),
		directoryPinSpec(),
		newProductionPathAPI(),
	)
	if chain != nil {
		_ = chain.close()
		t.Fatal("openPinnedChainWith() followed a reparse point")
	}
	assertFilesystemCode(t, err, protocol.CodeUnsafeReparsePoint)
	got, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(got) != "unchanged" {
		t.Fatalf("external sentinel = %q, error = %v", got, readErr)
	}
}

func TestWindows_CaseSensitiveDirectoryFailsClosed(t *testing.T) {
	target := t.TempDir()
	api := newProductionPathAPI()
	handle, err := api.openPath(nativeWindowsPath(target), openSpec{
		access:    windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES | windows.SYNCHRONIZE,
		share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE,
		creation:  windows.OPEN_EXISTING,
		options:   windows.FILE_FLAG_BACKUP_SEMANTICS | windows.FILE_FLAG_OPEN_REPARSE_POINT,
		directory: true,
	})
	if err != nil {
		t.Skipf("case-sensitivity probe open unavailable: %v", err)
	}
	t.Cleanup(func() {
		information := fileCaseSensitiveInfo{}
		_ = setFileInformationWindows(
			handle,
			fileCaseSensitiveClass,
			unsafe.Pointer(&information),
			unsafe.Sizeof(information),
		)
		_ = api.closeHandle(handle)
	})
	information := fileCaseSensitiveInfo{flags: fileCaseSensitiveDir}
	if err := setFileInformationWindows(
		handle,
		fileCaseSensitiveClass,
		unsafe.Pointer(&information),
		unsafe.Sizeof(information),
	); err != nil {
		t.Skipf("per-directory case sensitivity unavailable: %v", err)
	}
	chain, err := openPinnedChainWith(
		t.Context(),
		mustCanonicalize(t, filepath.Dir(target)),
		mustCanonicalize(t, target),
		directoryPinSpec(),
		api,
	)
	if chain != nil {
		_ = chain.close()
		t.Fatal("case-sensitive directory was accepted")
	}
	if !errors.Is(err, ErrUnsupportedCaseSensitivity) {
		t.Fatalf("error = %v, want ErrUnsupportedCaseSensitivity", err)
	}
}

func TestWindows_UNCAndRemoteRootsFailClosed(t *testing.T) {
	api := newProductionPathAPI()
	api.driveType = func(string) (uint32, error) {
		return windows.DRIVE_REMOTE, nil
	}
	err := ensureLocalVolumeWith(mustCanonicalize(t, t.TempDir()), api)
	if err == nil {
		t.Fatal("remote root was accepted")
	}
	t.Skip("UNC share fixture unavailable; remote drive classification completed")
}

func TestWindows_RenameInformationUsesPinnedParent(t *testing.T) {
	parentPath := t.TempDir()
	sourcePath := filepath.Join(parentPath, "source")
	if err := os.WriteFile(sourcePath, []byte("source"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	parentUTF16, err := windows.UTF16PtrFromString(parentPath)
	if err != nil {
		t.Fatalf("windows.UTF16PtrFromString(parent) error = %v", err)
	}
	parent, err := windows.CreateFile(
		parentUTF16,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		t.Fatalf("windows.CreateFile(parent) error = %v", err)
	}
	t.Cleanup(func() {
		if err := windows.CloseHandle(parent); err != nil {
			t.Errorf("windows.CloseHandle(parent) error = %v", err)
		}
	})
	openSource := func(name string) (windows.Handle, func()) {
		t.Helper()
		handle, err := openRelativeWindows(parent, name, openSpec{
			access:    windows.FILE_READ_ATTRIBUTES | windows.DELETE | windows.SYNCHRONIZE,
			share:     windows.FILE_SHARE_READ | windows.FILE_SHARE_DELETE,
			creation:  windows.OPEN_EXISTING,
			options:   windows.FILE_FLAG_OPEN_REPARSE_POINT,
			directory: false,
		})
		if err != nil {
			t.Fatalf("openRelativeWindows(%s) error = %v", name, err)
		}
		closed := false
		t.Cleanup(func() {
			if !closed {
				if err := windows.CloseHandle(handle); err != nil {
					t.Errorf("windows.CloseHandle(%s) error = %v", name, err)
				}
			}
		})
		return handle, func() {
			t.Helper()
			if closed {
				return
			}
			if err := windows.CloseHandle(handle); err != nil {
				t.Fatalf("windows.CloseHandle(%s) error = %v", name, err)
			}
			closed = true
		}
	}
	source, closeSource := openSource("source")

	if err := renameStateWindows(source, parent, "target", 0); err != nil {
		t.Fatalf("renameStateWindows() error = %v", err)
	}
	closeSource()
	got, err := os.ReadFile(filepath.Join(parentPath, "target"))
	if err != nil {
		t.Fatalf("os.ReadFile(target) error = %v", err)
	}
	if string(got) != "source" {
		t.Fatalf("target content = %q, want source", got)
	}

	collisionSourcePath := filepath.Join(parentPath, "collision-source")
	if err := os.WriteFile(collisionSourcePath, []byte("collision-source"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(collision source) error = %v", err)
	}
	occupiedPath := filepath.Join(parentPath, "occupied")
	if err := os.WriteFile(occupiedPath, []byte("occupied"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(occupied) error = %v", err)
	}
	collisionSource, closeCollisionSource := openSource("collision-source")
	collisionErr := renameStateWindows(collisionSource, parent, "occupied", 0)
	closeCollisionSource()
	if !errors.Is(collisionErr, windows.ERROR_ALREADY_EXISTS) &&
		!errors.Is(collisionErr, windows.ERROR_FILE_EXISTS) {
		t.Fatalf("collision error = %v, want already exists", collisionErr)
	}
	occupied, err := os.ReadFile(occupiedPath)
	if err != nil {
		t.Fatalf("os.ReadFile(occupied) error = %v", err)
	}
	if string(occupied) != "occupied" {
		t.Fatalf("occupied content = %q, want occupied", occupied)
	}
	collisionSourceBytes, err := os.ReadFile(collisionSourcePath)
	if err != nil {
		t.Fatalf("os.ReadFile(collision source) error = %v", err)
	}
	if string(collisionSourceBytes) != "collision-source" {
		t.Fatalf("collision source content = %q, want collision-source", collisionSourceBytes)
	}

	replaceSourcePath := filepath.Join(parentPath, "replace-source")
	if err := os.WriteFile(replaceSourcePath, []byte("replacement"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(replace source) error = %v", err)
	}
	replaceTargetPath := filepath.Join(parentPath, "replace-target")
	if err := os.WriteFile(replaceTargetPath, []byte("replaced"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(replace target) error = %v", err)
	}
	replaceSource, closeReplaceSource := openSource("replace-source")
	if err := renameWindows(replaceSource, parent, "replace-target", true); err != nil {
		t.Fatalf("renameWindows(replace) error = %v", err)
	}
	closeReplaceSource()
	replaced, err := os.ReadFile(replaceTargetPath)
	if err != nil {
		t.Fatalf("os.ReadFile(replace target) error = %v", err)
	}
	if string(replaced) != "replacement" {
		t.Fatalf("replace target content = %q, want replacement", replaced)
	}
}

func TestWindows_CustomFileInformationStructuresMatchABI(t *testing.T) {
	if unsafe.Sizeof(ntObjectAttributes{}) != 48 {
		t.Fatalf("ntObjectAttributes size = %d, want 48", unsafe.Sizeof(ntObjectAttributes{}))
	}
	if unsafe.Offsetof(fileIDBothDirectoryInformation{}.fileName) != 104 {
		t.Fatalf(
			"directory fileName offset = %d, want 104",
			unsafe.Offsetof(fileIDBothDirectoryInformation{}.fileName),
		)
	}
	if unsafe.Sizeof(fileDispositionInfoEx{}) != 4 {
		t.Fatalf("fileDispositionInfoEx size = %d, want 4", unsafe.Sizeof(fileDispositionInfoEx{}))
	}
	if unsafe.Offsetof(fileRenameInformation{}.rootDirectory) != 8 {
		t.Fatalf(
			"fileRenameInformation rootDirectory offset = %d, want 8",
			unsafe.Offsetof(fileRenameInformation{}.rootDirectory),
		)
	}
	if unsafe.Offsetof(fileRenameInformation{}.fileName) != 20 {
		t.Fatalf(
			"fileRenameInformation fileName offset = %d, want 20",
			unsafe.Offsetof(fileRenameInformation{}.fileName),
		)
	}
}

func openPinnedTestChain(t *testing.T) *pinnedChain {
	t.Helper()
	parent := t.TempDir()
	target := filepath.Join(parent, "app")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("os.Mkdir() error = %v", err)
	}
	chain, err := openPinnedChainWith(
		t.Context(),
		mustCanonicalize(t, parent),
		mustCanonicalize(t, target),
		directoryPinSpec(),
		newProductionPathAPI(),
	)
	if err != nil {
		t.Fatalf("openPinnedChainWith() error = %v", err)
	}
	return chain
}

func newPathTestLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	return layout
}

func assertFilesystemCode(t *testing.T, err error, want protocol.Code) {
	t.Helper()
	var filesystemErr *Error
	if !errors.As(err, &filesystemErr) {
		t.Fatalf("error = %v, want *Error", err)
	}
	if got := filesystemErr.Code(); got != want {
		t.Fatalf("Code() = %q, want %q", got, want)
	}
}
