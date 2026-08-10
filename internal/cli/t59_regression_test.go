package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

func TestEnvironmentEnsure_OfflineCacheMissReturnsNetworkUnavailable(t *testing.T) {
	root := t.TempDir()
	layout := t59Layout(t, root, root)
	downloader := &t59CountingDownloader{err: errors.New("offline attempted downloader")}
	bootstrapper, err := uv.NewBootstrapper(layout, uv.WithDownloader(downloader))
	if err != nil {
		t.Fatalf("uv.NewBootstrapper() error = %v", err)
	}
	environment := &t59BootstrapEnvironment{bootstrap: bootstrapper}
	stdout, stderr, exitCode := t59Execute(
		t,
		root,
		strings.NewReader(""),
		[]string{"--app-root", root, "--offline", "--output", "ndjson", "environment", "ensure"},
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) { return environment, nil }),
	)
	if exitCode != protocol.ExitCodeNetworkFailure {
		t.Fatalf("Execute() exit code = %d, want %d; stderr=%q", exitCode, protocol.ExitCodeNetworkFailure, stderr)
	}
	if stderr != "" {
		t.Fatalf("Execute() stderr = %q, want empty", stderr)
	}
	errorEvent, resultEvent := t59FailureEvents(t, stdout)
	t59AssertFailureTuple(t, errorEvent, resultEvent, protocol.CodeNetworkUnavailable, protocol.StageUVDownload)
	if downloader.calls != 0 {
		t.Fatalf("downloader calls = %d, want zero in offline mode", downloader.calls)
	}
	t59AssertPathAbsent(t, layout.MutationStateFile())
	versionDir, err := layout.UVVersionDir(uv.FixedVersion)
	if err != nil {
		t.Fatalf("UVVersionDir() error = %v", err)
	}
	t59AssertPathAbsent(t, versionDir)
	t59AssertProtectedPathsAbsent(t, layout)
}

func TestEnvironmentEnsure_OfflineCacheHitSkipsNetwork(t *testing.T) {
	root := t.TempDir()
	layout := t59Layout(t, root, root)
	executable, err := layout.UVExecutable(uv.FixedVersion)
	if err != nil {
		t.Fatalf("UVExecutable() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatalf("MkdirAll(uv version) error = %v", err)
	}
	if err := os.WriteFile(executable, []byte("fake uv"), 0o700); err != nil {
		t.Fatalf("WriteFile(uv.exe) error = %v", err)
	}
	downloader := &t59CountingDownloader{err: errors.New("cache hit attempted downloader")}
	bootstrapper, err := uv.NewBootstrapper(
		layout,
		uv.WithDownloader(downloader),
		uv.WithVersionChecker(t59VersionChecker{}),
	)
	if err != nil {
		t.Fatalf("uv.NewBootstrapper() error = %v", err)
	}
	stdout, stderr, exitCode := t59Execute(
		t,
		root,
		strings.NewReader(""),
		[]string{"--app-root", root, "--offline", "--output", "ndjson", "environment", "ensure"},
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) {
			return &t59BootstrapEnvironment{bootstrap: bootstrapper}, nil
		}),
	)
	if exitCode != protocol.ExitCodeSuccess || stderr != "" {
		t.Fatalf("Execute() = %d, stderr=%q, want success", exitCode, stderr)
	}
	events := parseNDJSON(t, stdout)
	if got := eventString(events[len(events)-1], "code"); got != string(protocol.CodeOK) {
		t.Fatalf("result code = %q, want OK", got)
	}
	if downloader.calls != 0 {
		t.Fatalf("downloader calls = %d, want zero for valid cache", downloader.calls)
	}
	t59AssertPathAbsent(t, layout.MutationStateFile())
}

func TestEnvironmentEnsure_OnlineDownloadFailureRemainsUVDownloadFailed(t *testing.T) {
	root := t.TempDir()
	layout := t59Layout(t, root, root)
	downloader := &t59CountingDownloader{err: errors.New("injected transport failure")}
	bootstrapper, err := uv.NewBootstrapper(layout, uv.WithDownloader(downloader))
	if err != nil {
		t.Fatalf("uv.NewBootstrapper() error = %v", err)
	}
	stdout, stderr, exitCode := t59Execute(
		t,
		root,
		strings.NewReader(""),
		[]string{"--app-root", root, "--output", "ndjson", "environment", "ensure"},
		WithEnvironmentFactory(func(*config.Layout) (environmentService, error) {
			return &t59BootstrapEnvironment{bootstrap: bootstrapper}, nil
		}),
	)
	if exitCode != protocol.ExitCodeNetworkFailure || stderr != "" {
		t.Fatalf("Execute() = %d, stderr=%q, want network failure with empty stderr", exitCode, stderr)
	}
	errorEvent, resultEvent := t59FailureEvents(t, stdout)
	t59AssertFailureTuple(t, errorEvent, resultEvent, protocol.CodeUVDownloadFailed, protocol.StageUVDownload)
	if downloader.calls == 0 {
		t.Fatal("downloader calls = 0, want online source attempts")
	}
	t59AssertPathAbsent(t, layout.MutationStateFile())
}

func TestEnvironmentEnsure_TerminalFailureRemovesMutation(t *testing.T) {
	tests := []struct {
		name          string
		uvErr         error
		waitForCancel bool
		removeErr     error
		wantExit      int
		wantCode      protocol.Code
		wantRetained  bool
	}{
		{name: "offline_cache_miss", uvErr: t59Failure(protocol.CodeNetworkUnavailable, protocol.StageUVDownload, false), wantExit: protocol.ExitCodeNetworkFailure, wantCode: protocol.CodeNetworkUnavailable},
		{name: "download_failure", uvErr: t59Failure(protocol.CodeUVDownloadFailed, protocol.StageUVDownload, false), wantExit: protocol.ExitCodeNetworkFailure, wantCode: protocol.CodeUVDownloadFailed},
		{name: "checksum_mismatch", uvErr: t59Failure(protocol.CodeUVChecksumMismatch, protocol.StageUVVerify, false), wantExit: protocol.ExitCodeGitFailure, wantCode: protocol.CodeUVChecksumMismatch},
		{name: "extract_failure", uvErr: t59Failure(protocol.CodeUVDownloadFailed, protocol.StageUVDownload, false), wantExit: protocol.ExitCodeNetworkFailure, wantCode: protocol.CodeUVDownloadFailed},
		{name: "publish_failure", uvErr: t59Failure(protocol.CodeDirectoryOccupied, protocol.StageUVDownload, false), wantExit: protocol.ExitCodeOperationConflict, wantCode: protocol.CodeDirectoryOccupied},
		{name: "version_mismatch", uvErr: t59Failure(protocol.CodeUVVersionMismatch, protocol.StageUVVerify, true), wantExit: protocol.ExitCodePreconditionFailed, wantCode: protocol.CodeUVVersionMismatch},
		{name: "success", wantExit: protocol.ExitCodeSuccess, wantCode: protocol.CodeOK},
		{name: "cancel", waitForCancel: true, wantExit: protocol.ExitCodeOperationCancelled, wantCode: protocol.CodeOperationCancelled},
		{name: "state_delete_failure", removeErr: errors.New("injected transaction delete failure"), wantExit: protocol.ExitCodeOperationConflict, wantCode: protocol.CodeStateWriteFailed, wantRetained: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			log := &m5TestLog{}
			environment := &m5TestEnvironment{
				calls:         &log.calls,
				uvErr:         test.uvErr,
				waitForCancel: test.waitForCancel,
			}
			store := &m5TestStateStore{calls: &log.calls, removeErr: test.removeErr}
			input := ""
			if test.waitForCancel {
				input = `{"protocol":1,"command":"cancel","commandId":"01J00000000000000000000009"}` + "\n"
			}
			stdout, stderr, exitCode := t59Execute(
				t,
				root,
				strings.NewReader(input),
				[]string{"--app-root", root, "--output", "ndjson", "environment", "ensure"},
				WithEnvironmentFactory(func(*config.Layout) (environmentService, error) { return environment, nil }),
				WithEnvironmentStateStoreFactory(func(context.Context, *config.Layout, func() time.Time) (environmentStateStore, error) {
					return store, nil
				}),
				WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
					return &m5TestCoordinator{calls: &log.calls}, nil
				}),
				WithWorkspaceLoggerFactory(func(context.Context, *config.Layout, io.Writer, string, string, func() time.Time) (workspaceLogger, error) {
					return log, nil
				}),
			)
			if exitCode != test.wantExit || stderr != "" {
				t.Fatalf("Execute() = %d, stderr=%q, want exit %d and empty stderr; stdout=%s", exitCode, stderr, test.wantExit, stdout)
			}
			events := parseNDJSON(t, stdout)
			if got := eventString(events[len(events)-1], "code"); got != string(test.wantCode) {
				t.Fatalf("result code = %q, want %q", got, test.wantCode)
			}
			if store.transactionActive != test.wantRetained {
				t.Fatalf("transaction active = %t, want %t", store.transactionActive, test.wantRetained)
			}
			if store.removeCalls != 1 {
				t.Fatalf("transaction remove calls = %d, want 1", store.removeCalls)
			}
			if store.removeContextErr != nil {
				t.Fatalf("transaction cleanup context error = %v, want independent live context", store.removeContextErr)
			}
		})
	}
}

func TestWorkspaceSync_GitHubOfficialWithMirrorOnlyReturnsInvalidArgument(t *testing.T) {
	root := t.TempDir()
	layout := t59Layout(t, root, root)
	before := t59DirectoryEntries(t, root)
	stdout, stderr, exitCode := t59Execute(
		t,
		root,
		strings.NewReader(""),
		[]string{
			"--app-root", root,
			"--output", "ndjson",
			"--mirror", "git=github",
			"--mirror-only",
			"workspace", "sync", "--version", "v5.3.1",
		},
	)
	if exitCode != protocol.ExitCodeInvalidArgument {
		t.Fatalf("Execute() exit code = %d, want %d; stderr=%q; stdout=%s", exitCode, protocol.ExitCodeInvalidArgument, stderr, stdout)
	}
	if stderr != "" {
		t.Fatalf("Execute() stderr = %q, want empty", stderr)
	}
	errorEvent, resultEvent := t59FailureEvents(t, stdout)
	t59AssertFailureTuple(t, errorEvent, resultEvent, protocol.CodeInvalidArgument, protocol.StageWorkspaceClone)
	after := t59DirectoryEntries(t, root)
	if strings.Join(after, "\x00") != strings.Join(before, "\x00") {
		t.Fatalf("policy rejection changed app-root entries: before=%v after=%v", before, after)
	}
	t59AssertPathAbsent(t, layout.RepoDir())
	t59AssertPathAbsent(t, layout.MutationStateFile())
	updates, err := filepath.Glob(filepath.Join(root, "repo.update-*"))
	if err != nil {
		t.Fatalf("Glob(repo.update-*) error = %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("repo update paths = %v, want none", updates)
	}
}

func TestEnvironmentCheck_MissingAppRootIsUninitializedAndReadOnly(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		base := t.TempDir()
		missing := filepath.Join(base, "missing-app-root")
		stdout, stderr, exitCode := t59Execute(
			t,
			base,
			strings.NewReader(""),
			[]string{"--app-root", missing, "--output", "ndjson", "environment", "check"},
		)
		if exitCode != protocol.ExitCodeSuccess || stderr != "" {
			t.Fatalf("Execute() = %d, stderr=%q, want success; stdout=%s", exitCode, stderr, stdout)
		}
		events := parseNDJSON(t, stdout)
		result := events[len(events)-1]
		if eventString(result, "code") != string(protocol.CodeOK) {
			t.Fatalf("result code = %q, want OK", eventString(result, "code"))
		}
		details, ok := result.object["details"].(map[string]any)
		if !ok || details["uvReady"] != false {
			t.Fatalf("result details = %#v, want uvReady=false", result.object["details"])
		}
		t59AssertPathAbsent(t, missing)
	})

	t.Run("ordinary file is not absence", func(t *testing.T) {
		base := t.TempDir()
		appRoot := filepath.Join(base, "app-root-file")
		if err := os.WriteFile(appRoot, []byte("sentinel"), 0o600); err != nil {
			t.Fatalf("WriteFile(app root) error = %v", err)
		}
		_, _, exitCode := t59Execute(
			t,
			base,
			strings.NewReader(""),
			[]string{"--app-root", appRoot, "--output", "ndjson", "environment", "check"},
		)
		if exitCode == protocol.ExitCodeSuccess {
			t.Fatal("Execute() exit code = success, want fail-closed ordinary-file rejection")
		}
		got, err := os.ReadFile(appRoot)
		if err != nil || string(got) != "sentinel" {
			t.Fatalf("app-root file = %q, %v, want unchanged sentinel", got, err)
		}
	})

	t.Run("reparse is not absence", func(t *testing.T) {
		base := t.TempDir()
		external := filepath.Join(base, "external")
		link := filepath.Join(base, "app-root-link")
		if err := os.Mkdir(external, 0o700); err != nil {
			t.Fatalf("Mkdir(external) error = %v", err)
		}
		sentinel := filepath.Join(external, "sentinel.txt")
		if err := os.WriteFile(sentinel, []byte("external"), 0o600); err != nil {
			t.Fatalf("WriteFile(sentinel) error = %v", err)
		}
		if err := os.Symlink(external, link); err != nil {
			t.Skipf("directory symlink unavailable: %v", err)
		}
		_, _, exitCode := t59Execute(
			t,
			base,
			strings.NewReader(""),
			[]string{"--app-root", link, "--output", "ndjson", "environment", "check"},
		)
		if exitCode == protocol.ExitCodeSuccess {
			t.Fatal("Execute() exit code = success, want fail-closed reparse rejection")
		}
		got, err := os.ReadFile(sentinel)
		if err != nil || string(got) != "external" {
			t.Fatalf("external sentinel = %q, %v, want unchanged", got, err)
		}
	})
}

type t59BootstrapEnvironment struct {
	m5UnsupportedEnvironment
	bootstrap *uv.Bootstrapper
}

func (e *t59BootstrapEnvironment) EnsureUV(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
) (string, error) {
	return e.bootstrap.Ensure(ctx, operationID, policy)
}

func (e *t59BootstrapEnvironment) EnsureUVWithLine(
	ctx context.Context,
	operationID string,
	policy mirror.Policy,
	line uv.LineFunc,
) (string, error) {
	return e.bootstrap.EnsureWithLine(ctx, operationID, policy, line)
}

func (e *t59BootstrapEnvironment) CheckUV(ctx context.Context) (bool, error) {
	return e.bootstrap.Check(ctx)
}

type t59CountingDownloader struct {
	calls int
	err   error
}

func (d *t59CountingDownloader) Download(
	context.Context,
	mirror.DownloadRequest,
) (mirror.DownloadResult, error) {
	d.calls++
	return mirror.DownloadResult{}, d.err
}

type t59VersionChecker struct{}

func (t59VersionChecker) Check(context.Context, string, string, uv.LineFunc) error { return nil }

type t59OperationError struct {
	code      protocol.Code
	stage     protocol.Stage
	committed bool
}

func t59Failure(code protocol.Code, stage protocol.Stage, committed bool) error {
	return &t59OperationError{code: code, stage: stage, committed: committed}
}

func (e *t59OperationError) Error() string           { return "injected t5.9 failure" }
func (e *t59OperationError) Code() protocol.Code     { return e.code }
func (e *t59OperationError) Stage() protocol.Stage   { return e.stage }
func (e *t59OperationError) Message() string         { return "注入的 T5.9 失败" }
func (e *t59OperationError) Details() map[string]any { return map[string]any{"committed": e.committed} }
func (e *t59OperationError) Committed() bool         { return e.committed }

func t59Execute(
	t *testing.T,
	cwd string,
	input io.Reader,
	args []string,
	options ...Option,
) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	values := append([]Option{WithCWD(cwd)}, options...)
	exitCode := Execute(context.Background(), args, IO{In: input, Out: &stdout, Err: &stderr}, values...)
	return stdout.String(), stderr.String(), exitCode
}

func t59FailureEvents(t *testing.T, stdout string) (parsedEvent, parsedEvent) {
	t.Helper()
	var errorEvent, resultEvent parsedEvent
	for _, event := range parseNDJSON(t, stdout) {
		switch eventType(event) {
		case string(protocol.TypeError):
			errorEvent = event
		case string(protocol.TypeResult):
			resultEvent = event
		}
	}
	if errorEvent.raw == "" || resultEvent.raw == "" {
		t.Fatalf("missing error/result event: error=%q result=%q", errorEvent.raw, resultEvent.raw)
	}
	return errorEvent, resultEvent
}

func t59AssertFailureTuple(
	t *testing.T,
	errorEvent parsedEvent,
	resultEvent parsedEvent,
	wantCode protocol.Code,
	wantStage protocol.Stage,
) {
	t.Helper()
	for _, field := range []string{"code", "stage", "retryable"} {
		if errorEvent.object[field] != resultEvent.object[field] {
			t.Fatalf("%s tuple mismatch: error=%v result=%v", field, errorEvent.object[field], resultEvent.object[field])
		}
	}
	if !equalStringSlices(errorEvent.object["remediation"], resultEvent.object["remediation"]) {
		t.Fatalf("remediation tuple mismatch: error=%v result=%v", errorEvent.object["remediation"], resultEvent.object["remediation"])
	}
	if eventString(resultEvent, "code") != string(wantCode) || eventString(resultEvent, "stage") != string(wantStage) {
		t.Fatalf("result tuple = code %q stage %q, want %q/%q", eventString(resultEvent, "code"), eventString(resultEvent, "stage"), wantCode, wantStage)
	}
	definition, ok := protocol.LookupErrorDefinition(wantCode)
	if !ok {
		t.Fatalf("LookupErrorDefinition(%q) missing", wantCode)
	}
	if resultEvent.object["retryable"] != definition.Retryable {
		t.Fatalf("retryable = %v, want %t", resultEvent.object["retryable"], definition.Retryable)
	}
	wantRemediation := make([]any, len(definition.Remediation))
	for index, remediation := range definition.Remediation {
		wantRemediation[index] = string(remediation)
	}
	gotRemediation, _ := resultEvent.object["remediation"].([]any)
	if len(gotRemediation) != len(wantRemediation) {
		t.Fatalf("remediation = %v, want %v", gotRemediation, wantRemediation)
	}
	for index := range wantRemediation {
		if gotRemediation[index] != wantRemediation[index] {
			t.Fatalf("remediation = %v, want %v", gotRemediation, wantRemediation)
		}
	}
}

func t59Layout(t *testing.T, appRoot, installRoot string) *config.Layout {
	t.Helper()
	layout, err := config.NewLayout(appRoot, installRoot)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	return layout
}

func t59AssertProtectedPathsAbsent(t *testing.T, layout *config.Layout) {
	t.Helper()
	for _, path := range []string{
		layout.ConfigDir(),
		layout.DataDir(),
		layout.HistoryDir(),
		layout.ScriptDir(),
		layout.DebugDir(),
		layout.PluginsDir(),
	} {
		t59AssertPathAbsent(t, path)
	}
}

func t59AssertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lstat(%q) error = %v, want absent", path, err)
	}
}

func t59DirectoryEntries(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", path, err)
	}
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Name())
	}
	return result
}
