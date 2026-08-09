package uv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestPython_ReadAndValidateSpec(t *testing.T) {
	projectDir := t.TempDir()
	writePythonProject(t, projectDir, "3.12.10\n", "[project]\nrequires-python = \">=3.12,<3.13\"\n")

	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	service, err := NewPythonService(layout, &fakePythonRunner{})
	if err != nil {
		t.Fatalf("NewPythonService() error = %v", err)
	}

	spec, err := service.ReadSpec(context.Background(), projectDir)
	if err != nil {
		t.Fatalf("ReadSpec() error = %v", err)
	}
	if got, want := spec.Version.String(), "3.12.10"; got != want {
		t.Fatalf("ReadSpec() version = %q, want %q", got, want)
	}
	if got, want := spec.RequiresPython, ">=3.12,<3.13"; got != want {
		t.Fatalf("ReadSpec() requires-python = %q, want %q", got, want)
	}

	writePythonProject(t, projectDir, "3.12\n", "[project]\nrequires-python = \">=3.12\"\n")
	_, err = service.ReadSpec(context.Background(), projectDir)
	assertPythonCode(t, err, protocol.CodePythonVersionInvalid)
}

func TestPython_RequiresPythonCompatibility(t *testing.T) {
	tests := []struct {
		name     string
		requires string
		wantCode protocol.Code
	}{
		{name: "compatible range", requires: ">=3.12,<3.13"},
		{name: "compatible wildcard", requires: "==3.12.*"},
		{name: "incompatible upper bound", requires: ">=3.13", wantCode: protocol.CodePythonVersionIncompatible},
		{name: "invalid requirement", requires: ">=3.12,", wantCode: protocol.CodePythonVersionIncompatible},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projectDir := t.TempDir()
			writePythonProject(t, projectDir, "3.12.10", "[project]\nrequires-python = \""+test.requires+"\"\n")
			root := t.TempDir()
			layout, err := config.NewLayout(root, filepath.Dir(root))
			if err != nil {
				t.Fatalf("NewLayout() error = %v", err)
			}
			service, err := NewPythonService(layout, &fakePythonRunner{})
			if err != nil {
				t.Fatalf("NewPythonService() error = %v", err)
			}
			_, err = service.ReadSpec(context.Background(), projectDir)
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("ReadSpec() error = %v", err)
				}
				return
			}
			assertPythonCode(t, err, test.wantCode)
		})
	}
}

func TestPython_ReadRequiresPythonAcceptsTomlKeyForms(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "quoted table and multiline basic string",
			content: "[\"project\"]\n\"requires-python\" = \"\"\"\n>=3.12,<3.13\n\"\"\" # trailing comment\n",
		},
		{
			name:    "dotted quoted key and literal string",
			content: "project.\"requires-python\" = '>=3.12,<3.13'\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projectDir := t.TempDir()
			writePythonProject(t, projectDir, "3.12.10", test.content)
			root := t.TempDir()
			layout, err := config.NewLayout(root, filepath.Dir(root))
			if err != nil {
				t.Fatalf("NewLayout() error = %v", err)
			}
			service, err := NewPythonService(layout, &fakePythonRunner{})
			if err != nil {
				t.Fatalf("NewPythonService() error = %v", err)
			}
			if _, err := service.ReadSpec(context.Background(), projectDir); err != nil {
				t.Fatalf("ReadSpec() error = %v", err)
			}
		})
	}
}

func TestPython_ReadRequiresPythonRejectsDuplicateTomlKeys(t *testing.T) {
	projectDir := t.TempDir()
	writePythonProject(t, projectDir, "3.12.10", "[project]\nrequires-python = \">=3.12\"\n\"requires-python\" = \">=3.12,<3.13\"\n")
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	service, err := NewPythonService(layout, &fakePythonRunner{})
	if err != nil {
		t.Fatalf("NewPythonService() error = %v", err)
	}
	_, err = service.ReadSpec(context.Background(), projectDir)
	assertPythonCode(t, err, protocol.CodePythonVersionIncompatible)
}

func TestPython_InstallArguments(t *testing.T) {
	projectDir := t.TempDir()
	writePythonProject(t, projectDir, "3.12.10", "[project]\nrequires-python = \">=3.12,<3.13\"\n")
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	runner := &fakePythonRunner{
		listOutput: `[{"version":"3.12.10"}]`,
		findOutput: "C:/runtime/python/3.12.10/python.exe\n",
	}
	service, err := NewPythonService(layout, runner)
	if err != nil {
		t.Fatalf("NewPythonService() error = %v", err)
	}
	result, err := service.Prepare(context.Background(), PythonRequest{ProjectDir: projectDir})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if got, want := result.Spec.Version.String(), "3.12.10"; got != want {
		t.Fatalf("Prepare() version = %q, want %q", got, want)
	}
	if got, want := len(runner.calls), 3; got != want {
		t.Fatalf("runner call count = %d, want %d", got, want)
	}
	wantInstall := []string{
		"python", "install", "3.12.10",
		"--managed-python", "--install-dir", layout.PythonDir(), "--no-bin", "--no-registry",
	}
	if !reflect.DeepEqual(runner.calls[1].args, wantInstall) {
		t.Fatalf("install args = %#v, want %#v", runner.calls[1].args, wantInstall)
	}
	if got, want := runner.calls[1].options.PythonInstallDir, layout.PythonDir(); got != want {
		t.Fatalf("install PythonInstallDir = %q, want %q", got, want)
	}
	if got, want := runner.calls[1].options.ProjectEnvDir, layout.VenvDir(); got != want {
		t.Fatalf("install ProjectEnvDir = %q, want %q", got, want)
	}
	if got, want := runner.calls[2].args, []string{
		"python", "find", "3.12.10", "--managed-python", "--no-python-downloads",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("find args = %#v, want %#v", got, want)
	}
}

func TestPython_MirrorPolicyRotatesSources(t *testing.T) {
	projectDir := t.TempDir()
	writePythonProject(t, projectDir, "3.12.10", "[project]\nrequires-python = \">=3.12,<3.13\"\n")
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	runner := &fakePythonRunner{
		listOutput:     `[{"version":"3.12.10"}]`,
		findOutput:     "C:/runtime/python/3.12.10/python.exe\n",
		installResults: []fakeRunnerResponse{{err: errors.New("first source failed")}, {}},
	}
	service, err := NewPythonService(layout, runner)
	if err != nil {
		t.Fatalf("NewPythonService() error = %v", err)
	}
	service.network = newTestNetworkExecutor(t)
	policy, err := mirror.NewPolicy(mirror.PolicySpec{Preferred: map[mirror.Kind]string{}})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	if _, err := service.Prepare(t.Context(), PythonRequest{ProjectDir: projectDir, MirrorPolicy: policy}); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if got, want := len(runner.calls), 4; got != want {
		t.Fatalf("runner calls = %d, want %d", got, want)
	}
	if got, want := runner.calls[1].options.Environment[uvPythonInstallMirrorEnv],
		"https://gh-proxy.com/https://github.com/astral-sh/python-build-standalone/releases/download"; got != want {
		t.Fatalf("first Python mirror = %q, want %q", got, want)
	}
	if got, want := runner.calls[2].options.Environment[uvPythonInstallMirrorEnv],
		"https://github.com/astral-sh/python-build-standalone/releases/download"; got != want {
		t.Fatalf("second Python mirror = %q, want %q", got, want)
	}
}

func TestPython_OfflineFailureMapsToNetworkUnavailable(t *testing.T) {
	projectDir := t.TempDir()
	writePythonProject(t, projectDir, "3.12.10", "[project]\nrequires-python = \">=3.12,<3.13\"\n")
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatalf("NewLayout() error = %v", err)
	}
	runner := &fakePythonRunner{
		listOutput:     `[{"version":"3.12.10"}]`,
		installResults: []fakeRunnerResponse{{err: errors.New("cache miss")}},
	}
	service, err := NewPythonService(layout, runner)
	if err != nil {
		t.Fatalf("NewPythonService() error = %v", err)
	}
	service.network = newTestNetworkExecutor(t)
	policy, err := mirror.NewPolicy(mirror.PolicySpec{Preferred: map[mirror.Kind]string{}, Offline: true})
	if err != nil {
		t.Fatalf("NewPolicy() error = %v", err)
	}
	_, err = service.Prepare(t.Context(), PythonRequest{ProjectDir: projectDir, MirrorPolicy: policy})
	assertPythonCode(t, err, protocol.CodeNetworkUnavailable)
	if got := runner.calls[1].options.Environment[uvOfflineEnv]; got != "1" {
		t.Fatalf("offline environment = %q, want 1", got)
	}
	if got := runner.calls[1].options.Environment[uvPythonInstallMirrorEnv]; got != "" {
		t.Fatalf("offline Python mirror = %q, want empty", got)
	}
}

type fakePythonCall struct {
	args    []string
	options RunOptions
}

type fakePythonRunner struct {
	listOutput     string
	findOutput     string
	installResults []fakeRunnerResponse
	calls          []fakePythonCall
}

func (r *fakePythonRunner) Run(_ context.Context, args []string, options RunOptions) (UVResult, error) {
	r.calls = append(r.calls, fakePythonCall{args: append([]string(nil), args...), options: options})
	switch args[1] {
	case "list":
		return UVResult{Stdout: r.listOutput}, nil
	case "install":
		if len(r.installResults) == 0 {
			return UVResult{}, nil
		}
		response := r.installResults[0]
		r.installResults = r.installResults[1:]
		return response.result, response.err
	case "find":
		return UVResult{Stdout: r.findOutput}, nil
	default:
		return UVResult{}, errors.New("unexpected fake uv command")
	}
}

func writePythonProject(t *testing.T, projectDir, version, pyproject string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(projectDir, ".python-version"), []byte(version), 0o600); err != nil {
		t.Fatalf("write .python-version: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "pyproject.toml"), []byte(pyproject), 0o600); err != nil {
		t.Fatalf("write pyproject.toml: %v", err)
	}
}

func assertPythonCode(t *testing.T, err error, want protocol.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %s", want)
	}
	var structured *Error
	if !errors.As(err, &structured) {
		t.Fatalf("error = %T %v, want *uv.Error", err, err)
	}
	if got := structured.Code(); got != want {
		t.Fatalf("error code = %q, want %q", got, want)
	}
}
