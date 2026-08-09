package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/gitrepo"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

func TestM5Component_FreshRootRepairAndProtectedInputs(t *testing.T) {
	appRoot := t.TempDir()
	layout, err := config.NewLayout(appRoot, appRoot)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	protected := seedM5ComponentProtectedFiles(t, appRoot)
	fixture := newM5ComponentGitFixture(t)
	fakeUV := buildM5ComponentFakeUV(t)
	configPath := writeM5ComponentFakeUVConfig(t, layout)
	recordPath := filepath.Join(t.TempDir(), "fake-uv-record.jsonl")
	t.Setenv("FAKE_UV_CONFIG", configPath)
	t.Setenv("FAKE_UV_RECORD", recordPath)

	options := m5ComponentOptions(t, appRoot, fixture, fakeUV)
	runM5ComponentCommand(t, appRoot, options, "bootstrap", "--version", fixture.version)
	assertM5ComponentReady(t, layout, fixture)
	assertM5ComponentProtectedFiles(t, appRoot, protected)

	repositorySnapshot := snapshotM5ComponentFiles(t, map[string]string{
		"python version": layout.PythonVersionFile(),
		"project":        layout.PyProjectFile(),
		"lockfile":       layout.UVLockFile(),
	})
	checkOutput := runM5ComponentCommand(t, appRoot, options, "dependencies", "check")
	assertM5ComponentResultDetail(t, checkOutput, "lockfileChecked", true)
	assertM5ComponentResultDetail(t, checkOutput, "synchronized", true)
	assertM5ComponentReady(t, layout, fixture)

	staleVenvFile := filepath.Join(layout.VenvDir(), "stale-before-repair.txt")
	if err := os.WriteFile(staleVenvFile, []byte("must be removed"), 0o600); err != nil {
		t.Fatalf("write stale venv marker: %v", err)
	}
	runM5ComponentCommand(t, appRoot, options, "repair")
	assertM5ComponentReady(t, layout, fixture)
	if _, err := os.Stat(staleVenvFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale venv marker stat error = %v, want not exist", err)
	}
	if info, err := os.Stat(layout.VenvDir()); err != nil || !info.IsDir() {
		t.Fatalf("managed venv after repair = %#v, %v, want directory", info, err)
	}
	assertM5ComponentFiles(t, repositorySnapshot)
	assertM5ComponentProtectedFiles(t, appRoot, protected)
	assertM5ComponentFakeUVInvocations(t, recordPath)
}

func TestM5Component_CutpointRerunMatrix(t *testing.T) {
	stages := []protocol.Stage{
		protocol.StageRepair,
		protocol.StageUVCheck,
		protocol.StageUVDownload,
		protocol.StageUVVerify,
		protocol.StagePythonCheck,
		protocol.StagePythonInstall,
		protocol.StageDependenciesCheck,
		protocol.StageDependenciesRebuild,
		protocol.StageDependenciesSync,
	}
	for _, stage := range stages {
		t.Run(string(stage), func(t *testing.T) {
			appRoot := t.TempDir()
			layout, err := config.NewLayout(appRoot, appRoot)
			if err != nil {
				t.Fatalf("config.NewLayout() error = %v", err)
			}
			if err := os.MkdirAll(layout.RepoDir(), 0o700); err != nil {
				t.Fatalf("MkdirAll(repo) error = %v", err)
			}
			if err := os.WriteFile(layout.PythonVersionFile(), []byte("3.12.10\n"), 0o600); err != nil {
				t.Fatalf("write .python-version: %v", err)
			}
			if err := os.WriteFile(
				layout.PyProjectFile(),
				[]byte("[project]\nrequires-python = \">=3.12,<3.13\"\n"),
				0o600,
			); err != nil {
				t.Fatalf("write pyproject.toml: %v", err)
			}
			seedM5ComponentCutpoint(t, layout, stage)

			log := &m5TestLog{}
			environment := &m5TestEnvironment{calls: &log.calls}
			options := []Option{
				WithCWD(appRoot),
				WithClock(func() time.Time { return time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC) }),
				WithEnvironmentFactory(func(*config.Layout) (environmentService, error) {
					return environment, nil
				}),
				WithWorkspaceFactory(func(*config.Layout) (workspaceService, error) {
					return &m5TestWorkspace{calls: &log.calls}, nil
				}),
				WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
					return &m5TestCoordinator{calls: &log.calls}, nil
				}),
				WithWorkspaceLoggerFactory(func(context.Context, *config.Layout, io.Writer, string, string, func() time.Time) (workspaceLogger, error) {
					return log, nil
				}),
			}
			runM5ComponentCommand(t, appRoot, options, "repair")
			assertM5ComponentReady(t, layout, &m5ComponentGitFixture{
				version: "v5.4.0",
				commit:  "0123456789abcdef0123456789abcdef01234567",
			})
		})
	}
}

func seedM5ComponentCutpoint(t *testing.T, layout *config.Layout, stage protocol.Stage) {
	t.Helper()
	store, err := state.NewStore(t.Context(), layout)
	if err != nil {
		t.Fatalf("state.NewStore(seed cutpoint) error = %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = store.Close()
		}
	}()
	ready, err := store.NewReadyEnvironment(
		"v5.4.0",
		"0123456789abcdef0123456789abcdef01234567",
	)
	if err != nil {
		t.Fatalf("NewReadyEnvironment() error = %v", err)
	}
	if err := store.WriteEnvironment(t.Context(), ready); err != nil {
		t.Fatalf("WriteEnvironment() error = %v", err)
	}
	transaction, err := store.NewTransaction(state.TransactionMutation, state.TransactionInput{
		OperationID:   "01J00000000000000000000010",
		Command:       "repair",
		PID:           ^uint32(0),
		TargetVersion: "v5.4.0",
		Stage:         stage,
	})
	if err != nil {
		t.Fatalf("NewTransaction() error = %v", err)
	}
	if err := store.WriteTransaction(t.Context(), state.TransactionMutation, transaction); err != nil {
		t.Fatalf("WriteTransaction() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("state store Close() error = %v", err)
	}
	closed = true
}

func m5ComponentOptions(
	t *testing.T,
	appRoot string,
	fixture *m5ComponentGitFixture,
	fakeUV string,
) []Option {
	t.Helper()
	return []Option{
		WithCWD(appRoot),
		WithClock(func() time.Time { return time.Date(2026, 8, 9, 9, 0, 0, 0, time.UTC) }),
		WithEnvironmentFactory(func(layout *config.Layout) (environmentService, error) {
			return newM5ComponentEnvironment(layout, fakeUV)
		}),
		WithWorkspaceFactory(func(layout *config.Layout) (workspaceService, error) {
			return &m5ComponentWorkspace{layout: layout, fixture: fixture}, nil
		}),
		WithEnvironmentStateStoreFactory(func(
			ctx context.Context,
			layout *config.Layout,
			clock func() time.Time,
		) (environmentStateStore, error) {
			store, err := state.NewStore(ctx, layout, state.WithClock(clock))
			if err != nil {
				t.Fatalf("state.NewStore(component command) error = %v", err)
			}
			return store, nil
		}),
		WithMutationCoordinatorFactory(func(context.Context, *config.Layout) (gitrepo.MutationCoordinator, error) {
			calls := make([]string, 0, 3)
			return &m5TestCoordinator{calls: &calls}, nil
		}),
		WithWorkspaceLoggerFactory(func(context.Context, *config.Layout, io.Writer, string, string, func() time.Time) (workspaceLogger, error) {
			return &m5TestLog{}, nil
		}),
	}
}

func runM5ComponentCommand(t *testing.T, appRoot string, options []Option, arguments ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args := append([]string{"--app-root", appRoot, "--output", "ndjson"}, arguments...)
	code := Execute(
		t.Context(),
		args,
		IO{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		options...,
	)
	if code != protocol.ExitCodeSuccess {
		t.Fatalf("Execute(%q) exit code = %d, want success; stderr=%q; stdout=%q", arguments, code, stderr.String(), stdout.String())
	}
	return stdout.String()
}

func assertM5ComponentResultDetail(t *testing.T, output, key string, want any) {
	t.Helper()
	events := parseNDJSON(t, output)
	for _, event := range events {
		if eventType(event) != string(protocol.TypeResult) {
			continue
		}
		details, ok := event.object["details"].(map[string]any)
		if !ok {
			t.Fatalf("result details = %#v, want object", event.object["details"])
		}
		if got := details[key]; got != want {
			t.Fatalf("result details[%q] = %#v, want %#v", key, got, want)
		}
		return
	}
	t.Fatal("result event is missing")
}

func newM5ComponentEnvironment(layout *config.Layout, fakeUV string) (environmentService, error) {
	executable, err := layout.UVExecutable(uv.FixedVersion)
	if err != nil {
		return nil, err
	}
	managedRunner, err := uv.NewRunner(uv.RunnerConfig{
		Executable:       executable,
		ProjectDir:       layout.RepoDir(),
		PythonInstallDir: layout.PythonDir(),
		ProjectEnvDir:    layout.VenvDir(),
		CacheDir:         layout.UVCacheDir(),
	})
	if err != nil {
		return nil, err
	}
	python, err := uv.NewPythonService(layout, managedRunner)
	if err != nil {
		return nil, err
	}
	dependencies, err := uv.NewDependenciesService(
		layout,
		managedRunner,
		m5ComponentRemover{layout: layout},
	)
	if err != nil {
		return nil, err
	}
	return uv.NewEnvironmentService(
		&m5ComponentUV{layout: layout, source: fakeUV, destination: executable},
		python,
		dependencies,
	)
}

type m5ComponentUV struct {
	layout      *config.Layout
	source      string
	destination string
}

func (u *m5ComponentUV) Ensure(ctx context.Context, _ string, _ mirror.Policy) (string, error) {
	if _, err := os.Lstat(u.destination); errors.Is(err, os.ErrNotExist) {
		if err := copyM5ComponentFile(u.source, u.destination); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if err := u.checkVersion(ctx); err != nil {
		return "", err
	}
	return u.destination, nil
}

func (u *m5ComponentUV) Repair(ctx context.Context, operationID string, policy mirror.Policy) (string, error) {
	if err := os.Remove(u.destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return u.Ensure(ctx, operationID, policy)
}

func (u *m5ComponentUV) Check(ctx context.Context) (bool, error) {
	info, err := os.Lstat(u.destination)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("component uv is not a regular file")
	}
	if err := u.checkVersion(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func (u *m5ComponentUV) checkVersion(ctx context.Context) error {
	runner, err := uv.NewRunner(uv.RunnerConfig{
		Executable:       u.destination,
		ProjectDir:       u.layout.AppRoot(),
		PythonInstallDir: u.layout.PythonDir(),
		ProjectEnvDir:    u.layout.VenvDir(),
		CacheDir:         u.layout.UVCacheDir(),
	})
	if err != nil {
		return err
	}
	return runner.CheckVersion(ctx, uv.FixedVersion, protocol.StageUVVerify, nil)
}

func copyM5ComponentFile(source, destination string) error {
	payload, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	return os.WriteFile(destination, payload, 0o700)
}

type m5ComponentRemover struct{ layout *config.Layout }

func (r m5ComponentRemover) RemoveTree(
	ctx context.Context,
	request filesystem.DeleteRequest,
) (filesystem.DeleteResult, error) {
	operator, err := filesystem.New(ctx, r.layout, m5ComponentAuditor{})
	if err != nil {
		return filesystem.DeleteResult{}, err
	}
	return operator.RemoveTree(ctx, request)
}

type m5ComponentAuditor struct{}

func (m5ComponentAuditor) RecordDeletion(context.Context, filesystem.DeleteAuditRecord) error {
	return nil
}

type m5ComponentGitFixture struct {
	path    string
	version string
	branch  string
	commit  string
}

func newM5ComponentGitFixture(t *testing.T) *m5ComponentGitFixture {
	t.Helper()
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	repository, err := git.PlainInit(seed, false)
	if err != nil {
		t.Fatalf("git.PlainInit() error = %v", err)
	}
	files := map[string]string{
		".python-version":  "3.12.10\n",
		"pyproject.toml":   "[project]\nrequires-python = \">=3.12,<3.13\"\n",
		"uv.lock":          "version = 1\n",
		"res/version.json": "{\"version\":\"v5.4.0\"}\n",
	}
	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatalf("repository.Worktree() error = %v", err)
	}
	for relative, contents := range files {
		path := filepath.Join(seed, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", relative, err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", relative, err)
		}
		if _, err := worktree.Add(filepath.FromSlash(relative)); err != nil {
			t.Fatalf("worktree.Add(%q) error = %v", relative, err)
		}
	}
	commit, err := worktree.Commit("M5 component fixture", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "AUTO-MAS component test",
			Email: "component@example.invalid",
			When:  time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC),
		},
	})
	if err != nil {
		t.Fatalf("worktree.Commit() error = %v", err)
	}
	branch := "release/v5.4.0"
	branchReference := plumbing.NewBranchReferenceName(branch)
	if err := repository.Storer.SetReference(plumbing.NewHashReference(branchReference, commit)); err != nil {
		t.Fatalf("SetReference(branch) error = %v", err)
	}
	if err := repository.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branchReference)); err != nil {
		t.Fatalf("SetReference(HEAD) error = %v", err)
	}
	return &m5ComponentGitFixture{
		path:    seed,
		version: "v5.4.0",
		branch:  branch,
		commit:  commit.String(),
	}
}

type m5ComponentWorkspace struct {
	layout  *config.Layout
	fixture *m5ComponentGitFixture
}

func (s *m5ComponentWorkspace) Check(context.Context) (gitrepo.CheckResult, error) {
	repository, err := git.PlainOpen(s.layout.RepoDir())
	if errors.Is(err, git.ErrRepositoryNotExists) {
		return gitrepo.CheckResult{Healthy: false, Reason: "missing"}, nil
	}
	if err != nil {
		return gitrepo.CheckResult{}, err
	}
	head, err := repository.Head()
	if err != nil {
		return gitrepo.CheckResult{}, err
	}
	return gitrepo.CheckResult{
		Healthy: head.Hash().String() == s.fixture.commit,
		Version: s.fixture.version,
		Branch:  s.fixture.branch,
		Commit:  head.Hash().String(),
		Source:  "fixture",
		Reason:  "ok",
	}, nil
}

func (s *m5ComponentWorkspace) Sync(
	ctx context.Context,
	request gitrepo.SyncRequest,
) (gitrepo.SyncResult, error) {
	if request.MutationLease == nil {
		return gitrepo.SyncResult{}, errors.New("component workspace requires a mutation lease")
	}
	if request.Target.Version() != s.fixture.version || request.Target.Branch() != s.fixture.branch {
		return gitrepo.SyncResult{}, errors.New("component workspace target is unexpected")
	}
	check, err := s.Check(ctx)
	if err != nil {
		return gitrepo.SyncResult{}, err
	}
	changed := false
	if !check.Healthy {
		_, err := git.PlainCloneContext(ctx, s.layout.RepoDir(), false, &git.CloneOptions{
			URL:           s.fixture.path,
			ReferenceName: plumbing.NewBranchReferenceName(s.fixture.branch),
			SingleBranch:  true,
		})
		if err != nil {
			return gitrepo.SyncResult{}, err
		}
		changed = true
		check, err = s.Check(ctx)
		if err != nil || !check.Healthy {
			return gitrepo.SyncResult{}, errors.Join(err, errors.New("component workspace clone is unhealthy"))
		}
	}
	revision, err := gitrepo.NewRevision(check.Version, check.Branch, check.Commit, check.Source)
	if err != nil {
		return gitrepo.SyncResult{}, err
	}
	return gitrepo.SyncResult{
		Revision: revision,
		Changed:  changed,
		Status:   protocol.StateEnvironmentBroken,
	}, nil
}

func buildM5ComponentFakeUV(t *testing.T) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}
	workspace := filepath.Clean(filepath.Join(workingDirectory, "..", ".."))
	name := "fakeuv.exe"
	if runtime.GOOS != "windows" {
		name = "fakeuv"
	}
	output := filepath.Join(t.TempDir(), name)
	command := exec.Command(
		"go",
		"build",
		"-buildvcs=false",
		"-o",
		output,
		filepath.Join(workspace, "testdata", "fakeuv"),
	)
	command.Dir = workspace
	if buildOutput, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build M5 fake uv: %v\n%s", err, buildOutput)
	}
	return output
}

func writeM5ComponentFakeUVConfig(t *testing.T, layout *config.Layout) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-uv-config.json")
	pythonExecutable := filepath.Join(layout.PythonDir(), "cpython-3.12.10", "python.exe")
	payload, err := json.Marshal(map[string]any{
		"exitCode": 99,
		"rules": []map[string]any{
			{
				"argumentsPrefix": []string{"--version"},
				"stdout":          []string{"uv " + uv.FixedVersion},
			},
			{
				"argumentsPrefix": []string{"python", "list"},
				"stdout":          []string{`[{"version":"3.12.10"}]`},
			},
			{
				"argumentsPrefix":   []string{"python", "install"},
				"createDirectories": []string{filepath.Dir(pythonExecutable)},
			},
			{
				"argumentsPrefix": []string{"python", "find"},
				"stdout":          []string{pythonExecutable},
			},
			{"argumentsPrefix": []string{"lock"}},
			{
				"argumentsPrefix":   []string{"sync"},
				"createDirectories": []string{layout.VenvDir()},
			},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal(fake uv config) error = %v", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("WriteFile(fake uv config) error = %v", err)
	}
	return path
}

func seedM5ComponentProtectedFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	protected := map[string][]byte{
		"config/settings.json":                 []byte("config"),
		"data/user.db":                         []byte("data"),
		"history/run.log":                      []byte("history"),
		"script/custom.py":                     []byte("script"),
		"debug/trace.log":                      []byte("debug"),
		"plugins/pypi/site-packages/plugin.py": []byte("plugin"),
	}
	for relative, contents := range protected {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", relative, err)
		}
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", relative, err)
		}
	}
	return protected
}

func assertM5ComponentProtectedFiles(t *testing.T, root string, want map[string][]byte) {
	t.Helper()
	for relative, wantContents := range want {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Errorf("ReadFile(protected %q) error = %v", relative, err)
			continue
		}
		if !bytes.Equal(got, wantContents) {
			t.Errorf("protected %q = %q, want %q", relative, got, wantContents)
		}
	}
}

func snapshotM5ComponentFiles(t *testing.T, paths map[string]string) map[string][]byte {
	t.Helper()
	snapshot := make(map[string][]byte, len(paths))
	for name, path := range paths {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", name, err)
		}
		snapshot[path] = contents
	}
	return snapshot
}

func assertM5ComponentFiles(t *testing.T, want map[string][]byte) {
	t.Helper()
	for path, wantContents := range want {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("ReadFile(%q) error = %v", path, err)
			continue
		}
		if !bytes.Equal(got, wantContents) {
			t.Errorf("file %q changed: got %q, want %q", path, got, wantContents)
		}
	}
}

func assertM5ComponentReady(t *testing.T, layout *config.Layout, fixture *m5ComponentGitFixture) {
	t.Helper()
	store, err := state.NewStore(t.Context(), layout)
	if err != nil {
		t.Fatalf("state.NewStore() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("state store Close() error = %v", err)
		}
	}()
	environment, err := store.ReadEnvironment(t.Context())
	if err != nil {
		t.Fatalf("ReadEnvironment() error = %v", err)
	}
	if environment.Status != protocol.StateReadyToStart ||
		environment.LastSuccessful.Version != fixture.version ||
		environment.LastSuccessful.Commit != fixture.commit {
		t.Fatalf("environment state = %#v, want ready fixture revision", environment)
	}
	if _, err := store.ReadTransaction(t.Context(), state.TransactionMutation); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("ReadTransaction() error = %v, want ErrNotFound", err)
	}
}

func assertM5ComponentFakeUVInvocations(t *testing.T, path string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("os.Open(fake uv record) error = %v", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("fake uv record Close() error = %v", err)
		}
	}()
	wants := map[string]bool{
		"--version":      false,
		"python list":    false,
		"python install": false,
		"python find":    false,
		"lock":           false,
		"sync":           false,
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record struct {
			Arguments []string `json:"arguments"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode fake uv record: %v", err)
		}
		joined := strings.Join(record.Arguments, " ")
		for _, argument := range record.Arguments {
			if argument == "pip" {
				t.Fatalf("fake uv invocation crossed plugin boundary: %q", joined)
			}
		}
		for prefix := range wants {
			if joined == prefix || strings.HasPrefix(joined, prefix+" ") {
				wants[prefix] = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan fake uv record: %v", err)
	}
	for prefix, seen := range wants {
		if !seen {
			t.Errorf("fake uv record did not contain %q invocation", prefix)
		}
	}
}
