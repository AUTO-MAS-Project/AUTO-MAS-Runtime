package backend

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/health"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

func TestBackendDevelopment_RequiresOnlyExplicitRepoContract(t *testing.T) {
	f := newBackendFixture(t)
	repo := newDevelopmentRepo(t)
	f.state.environmentErr = errors.New("development must not read environment.json")
	f.repository.err = errors.New("development must not inspect git revision")
	f.proc.keepAlive = true

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(ctx, developmentRequest(f.request(), repo)) }()
	waitFor(t, f.emitter.running)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want context.Canceled", err)
	}
	if f.state.readCalls != 0 {
		t.Fatalf("ReadEnvironment calls = %d, want 0", f.state.readCalls)
	}
	if f.repository.checks != 0 {
		t.Fatalf("Repository checks = %d, want 0", f.repository.checks)
	}
	if f.state.beginInput.Version != "" {
		t.Fatalf("development transaction version = %q, want empty", f.state.beginInput.Version)
	}
	if len(f.health.expectations) != 1 || f.health.expectations[0].Mode != health.ModeDevelopment || f.health.expectations[0].Version != "" || f.health.expectations[0].Commit != "" {
		t.Fatalf("development health expectations = %#v, want protocol-only development", f.health.expectations)
	}
}

func TestBackendDevelopment_UsesExistingVenvWithoutSync(t *testing.T) {
	f := newBackendFixture(t)
	repo := newDevelopmentRepo(t)
	f.proc.keepAlive = true

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(ctx, developmentRequest(f.request(), repo)) }()
	waitFor(t, f.emitter.running)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want context.Canceled", err)
	}
	wantArgs := []string{"run", "--project", repo, "--no-sync", "main.py"}
	if !equalStrings(f.uv.args, wantArgs) {
		t.Fatalf("uv args = %#v, want %#v", f.uv.args, wantArgs)
	}
	if f.uv.options.Identity != nil {
		t.Fatalf("development identity = %#v, want nil", f.uv.options.Identity)
	}
	wantVenv := filepath.Join(repo, ".venv")
	if got := f.uv.options.ProjectEnvDir; got != wantVenv {
		t.Fatalf("ProjectEnvDir = %q, want %q", got, wantVenv)
	}
}

func TestBackendDevelopment_InjectsOnlyRequiredEnv(t *testing.T) {
	f := newBackendFixture(t)
	repo := newDevelopmentRepo(t)
	f.proc.keepAlive = true

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(ctx, developmentRequest(f.request(), repo)) }()
	waitFor(t, f.emitter.running)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want context.Canceled", err)
	}
	if f.uv.options.Identity != nil {
		t.Fatalf("development must not inject expected identity: %#v", f.uv.options.Identity)
	}
	for key := range f.uv.options.Environment {
		if key == "AUTO_MAS_EXPECTED_VERSION" || key == "AUTO_MAS_EXPECTED_COMMIT" {
			t.Fatalf("development injected forbidden environment key %q", key)
		}
	}
	runner := &uv.UVRunner{
		Executable:       filepath.Join(repo, "uv.exe"),
		ProjectDir:       repo,
		ProjectEnvDir:    filepath.Join(repo, ".venv"),
		PythonInstallDir: filepath.Join(repo, "python"),
		CacheDir:         filepath.Join(repo, "cache"),
	}
	environment := runner.EnvironmentForTesting(uv.RunOptions{
		ProjectDir:    repo,
		ProjectEnvDir: filepath.Join(repo, ".venv"),
		Environment: map[string]string{
			"AUTO_MAS_EXPECTED_VERSION": "stale-version",
			"AUTO_MAS_EXPECTED_COMMIT":  "stale-commit",
			"AUTO_MAS_SUPERVISED":       "stale-supervised",
		},
	})
	for _, forbidden := range []string{"AUTO_MAS_EXPECTED_VERSION", "AUTO_MAS_EXPECTED_COMMIT"} {
		if _, ok := environment[forbidden]; ok {
			t.Fatalf("resolved environment contains forbidden %q: %#v", forbidden, environment)
		}
	}
	if environment["UV_PROJECT_ENVIRONMENT"] != filepath.Join(repo, ".venv") {
		t.Fatalf("resolved UV_PROJECT_ENVIRONMENT = %q, want %q", environment["UV_PROJECT_ENVIRONMENT"], filepath.Join(repo, ".venv"))
	}
}

func TestBackendDevelopment_AllowsDirtyOutsideManagedRepo(t *testing.T) {
	f := newBackendFixture(t)
	repo := newDevelopmentRepo(t)
	for _, name := range []string{".git", "__pycache__", "untracked.txt", ".python-version", "uv.lock"} {
		path := filepath.Join(repo, name)
		if name == ".git" || name == "__pycache__" {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatalf("Mkdir(%q) error = %v", path, err)
			}
			continue
		}
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", path, err)
		}
	}
	f.proc.keepAlive = true
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(ctx, developmentRequest(f.request(), repo)) }()
	waitFor(t, f.emitter.running)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() error = %v, want context.Canceled", err)
	}
	if f.repository.checks != 0 || f.state.readCalls != 0 {
		t.Fatalf("managed-only checks ran: repository=%d environment=%d", f.repository.checks, f.state.readCalls)
	}
}

func TestBackendDevelopment_SnapshotUnchangedAfterStartupShutdown(t *testing.T) {
	f := newBackendFixture(t)
	repo := newDevelopmentRepo(t)
	for _, directory := range []string{".git", "__pycache__"} {
		if err := os.Mkdir(filepath.Join(repo, directory), 0o755); err != nil {
			t.Fatalf("Mkdir(%q) error = %v", directory, err)
		}
	}
	files := map[string]string{
		".git/HEAD":              "ref: refs/heads/main\n",
		".git/config":            "[core]\n\trepositoryformatversion = 0\n",
		"__pycache__/module.pyc": "compiled fixture bytes\n",
		"untracked.txt":          "untracked fixture content\n",
		"dirty.py":               "print('dirty fixture')\n",
		"main.py":                "print('dirty main')\n",
		"pyproject.toml":         "[project]\nname = 'dirty-fixture'\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(name)), []byte(contents), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", name, err)
		}
	}
	snapshot := snapshotDevelopmentTree(t, repo)
	f.proc.keepAlive = true
	mailbox := NewControlMailbox(8)
	t.Cleanup(mailbox.Close)
	request := developmentRequest(f.request(), repo)
	request.Control = mailbox
	done := make(chan error, 1)
	go func() { done <- f.supervisor().Supervise(t.Context(), request) }()
	waitFor(t, f.emitter.running)
	if err := mailbox.Submit(t.Context(), protocol.ControlCommand{
		Command:   protocol.ControlShutdown,
		CommandID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
	}); err != nil {
		t.Fatalf("Submit(shutdown) error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Supervise() error = %v, want nil", err)
	}
	if got := snapshotDevelopmentTree(t, repo); !equalStrings(got, snapshot) {
		t.Fatalf("development tree changed: got %#v, want %#v", got, snapshot)
	}
}

func TestBackendDevelopment_UsesSameShutdownRestartRules(t *testing.T) {
	t.Run("shutdown", func(t *testing.T) {
		f := newBackendFixture(t)
		repo := newDevelopmentRepo(t)
		f.proc.keepAlive = true
		mailbox := NewControlMailbox(8)
		request := developmentRequest(f.request(), repo)
		request.Control = mailbox

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- f.supervisor().Supervise(ctx, request) }()
		waitFor(t, f.emitter.running)
		if err := mailbox.Submit(t.Context(), protocol.ControlCommand{Command: protocol.ControlShutdown, CommandID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}); err != nil {
			t.Fatalf("Submit(shutdown) error = %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("Supervise() error = %v, want nil", err)
		}
		if f.uv.startCalls != 1 {
			t.Fatalf("StartManaged calls = %d, want 1", f.uv.startCalls)
		}
		if !f.proc.terminated || !f.proc.closed {
			// HTTP is intentionally absent in this fixture; the shared force path must close the Job.
			t.Fatalf("development shutdown cleanup = terminated:%v closed:%v", f.proc.terminated, f.proc.closed)
		}
	})

	t.Run("one restart then no third spawn", func(t *testing.T) {
		f := newBackendFixture(t)
		repo := newDevelopmentRepo(t)
		second := &fakeProcess{pid: 5252, keepAlive: true}
		f.proc.keepAlive = true
		f.uv.procSequence = []ManagedProcess{f.proc, second}
		f.uv.startNotify = make(chan struct{}, 2)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- f.supervisor().Supervise(ctx, developmentRequest(f.request(), repo)) }()
		waitFor(t, f.emitter.running)
		f.proc.Exit()
		select {
		case <-f.uv.startNotify:
		case <-time.After(time.Second):
			t.Fatal("restart did not spawn a second process")
		}
		second.Exit()
		select {
		case err := <-done:
			assertBackendCode(t, err, protocol.CodeBackendExitedUnexpectedly)
		case <-time.After(time.Second):
			t.Fatal("second unexpected exit did not terminate supervision")
		}
		if f.uv.startCalls != 2 {
			t.Fatalf("StartManaged calls = %d, want exactly 2", f.uv.startCalls)
		}
	})

	t.Run("restart rechecks repository files", func(t *testing.T) {
		f := newBackendFixture(t)
		repo := newDevelopmentRepo(t)
		f.proc.keepAlive = true
		f.proc.cleanupStarted = make(chan struct{})
		f.proc.cleanupRelease = make(chan struct{})

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- f.supervisor().Supervise(ctx, developmentRequest(f.request(), repo)) }()
		waitFor(t, f.emitter.running)
		f.proc.Exit()
		select {
		case <-f.proc.cleanupStarted:
		case <-time.After(time.Second):
			t.Fatal("first process cleanup did not start")
		}
		if err := os.Remove(filepath.Join(repo, "pyproject.toml")); err != nil {
			t.Fatalf("Remove(pyproject.toml) error = %v", err)
		}
		close(f.proc.cleanupRelease)
		select {
		case err := <-done:
			assertBackendCode(t, err, protocol.CodeBackendRestartFailed)
		case <-time.After(time.Second):
			t.Fatal("restart preflight did not fail after repository change")
		}
		if f.uv.startCalls != 1 {
			t.Fatalf("StartManaged calls = %d, want no restart spawn", f.uv.startCalls)
		}
	})
}

func TestBackendDevelopment_MapsPreconditionErrors(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*backendFixture, string)
		want  protocol.Code
	}{
		{name: "missing repo", setup: func(_ *backendFixture, _ string) {}, want: protocol.CodeInvalidArgument},
		{name: "missing main", setup: func(_ *backendFixture, repo string) { _ = os.Remove(filepath.Join(repo, "main.py")) }, want: protocol.CodeBackendEntryNotFound},
		{name: "main directory", setup: func(_ *backendFixture, repo string) {
			_ = os.Remove(filepath.Join(repo, "main.py"))
			_ = os.Mkdir(filepath.Join(repo, "main.py"), 0o755)
		}, want: protocol.CodeBackendEntryNotFound},
		{name: "missing pyproject", setup: func(_ *backendFixture, repo string) { _ = os.Remove(filepath.Join(repo, "pyproject.toml")) }, want: protocol.CodeEnvironmentBroken},
		{name: "pyproject directory", setup: func(_ *backendFixture, repo string) {
			_ = os.Remove(filepath.Join(repo, "pyproject.toml"))
			_ = os.Mkdir(filepath.Join(repo, "pyproject.toml"), 0o755)
		}, want: protocol.CodeEnvironmentBroken},
		{name: "missing venv", setup: func(_ *backendFixture, repo string) { _ = os.RemoveAll(filepath.Join(repo, ".venv")) }, want: protocol.CodeEnvironmentBroken},
		{name: "venv file", setup: func(_ *backendFixture, repo string) {
			_ = os.RemoveAll(filepath.Join(repo, ".venv"))
			_ = os.WriteFile(filepath.Join(repo, ".venv"), []byte("not a directory"), 0o644)
		}, want: protocol.CodeEnvironmentBroken},
		{name: "uv failure", setup: func(f *backendFixture, _ string) {
			f.uv.checkErr = &fakeCodeError{code: protocol.CodeUVVersionMismatch}
		}, want: protocol.CodeUVVersionMismatch},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			f := newBackendFixture(t)
			repo := newDevelopmentRepo(t)
			if test.name == "missing repo" {
				repo = filepath.Join(t.TempDir(), "not-found")
			}
			test.setup(f, repo)
			assertBackendCode(t, f.supervisor().Supervise(t.Context(), developmentRequest(f.request(), repo)), test.want)
			if f.uv.startCalls != 0 {
				t.Fatalf("StartManaged calls = %d, want 0", f.uv.startCalls)
			}
		})
	}
}

func TestBackendDevelopment_RejectsRuntimeRootInsideRepoBeforeSideEffects(t *testing.T) {
	t.Run("external repo and not-yet-created app root are allowed", func(t *testing.T) {
		repo := newDevelopmentRepo(t)
		f := newBackendFixture(t)
		missingRoot := filepath.Join(t.TempDir(), "runtime-root-not-created")
		layout, err := config.NewLayout(missingRoot, filepath.Dir(missingRoot))
		if err != nil {
			t.Fatalf("NewLayout() error = %v", err)
		}
		f.layout = layout
		f.proc.keepAlive = true
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- f.supervisor().Supervise(ctx, developmentRequest(f.request(), repo)) }()
		waitFor(t, f.emitter.running)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Supervise() error = %v, want context.Canceled", err)
		}
		if _, err := os.Stat(repo); err != nil {
			t.Fatalf("development repo disappeared: %v", err)
		}
		if _, err := os.Stat(missingRoot); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Runtime root was created: %v", err)
		}
	})

	repo := newDevelopmentRepo(t)
	root := filepath.Join(repo, "runtime")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir(runtime root) error = %v", err)
	}
	f := newBackendFixture(t)
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	f.layout = layout
	f.state.environmentErr = errors.New("must not read environment")
	request := developmentRequest(f.request(), repo)
	err = f.supervisor().Supervise(t.Context(), request)
	assertBackendCode(t, err, protocol.CodeInvalidArgument)
	var backendErr *Error
	if !errors.As(err, &backendErr) || backendErr.Details()["reason"] != "runtime_root_inside_development_repo" {
		t.Fatalf("error details = %#v, want runtime_root_inside_development_repo", backendErr)
	}
	if f.lock.acquireStarted != nil || f.lock.closeCalls != 0 || f.state.closeCalls != 0 || f.logger.closeCalls != 0 || f.uv.startCalls != 0 {
		t.Fatalf("side effects before containment rejection: lock=%d state=%d logger=%d uv=%d", f.lock.closeCalls, f.state.closeCalls, f.logger.closeCalls, f.uv.startCalls)
	}
}

func developmentRequest(request Request, repo string) Request {
	request.Mode = ModeDevelopment
	request.DevelopmentRepo = repo
	return request
}

func newDevelopmentRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, name := range []string{"main.py", "pyproject.toml"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("# fixture\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(repo, ".venv"), 0o755); err != nil {
		t.Fatalf("Mkdir(.venv) error = %v", err)
	}
	return repo
}

func snapshotDevelopmentTree(t *testing.T, root string) []string {
	t.Helper()
	var snapshot []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			rel = ""
		}
		digest := ""
		if info.Mode().IsRegular() {
			payload, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			digest = fmt.Sprintf("%x", sha256.Sum256(payload))
		}
		snapshot = append(snapshot, fmt.Sprintf("%s|%s|%d|%o|%s", rel, info.Mode().Type(), info.Size(), info.Mode().Perm(), digest))
		return nil
	})
	if err != nil {
		t.Fatalf("Walk(%q) error = %v", root, err)
	}
	return snapshot
}
