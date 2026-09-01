package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

// m13ComponentLock 是组件夹具使用的 uv.lock：形态与真实锁一致，含两处官方前缀，
// 因此镜像改写、回退与「repo 字节不变」都能在真实文件上断言。
const m13ComponentLock = `version = 1
revision = 2
requires-python = ">=3.12, <3.13"

[[package]]
name = "certifi"
version = "2025.1.31"
source = { registry = "https://pypi.org/simple" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/38/fc/certifi-2025.1.31-py3-none-any.whl", hash = "sha256:ca78db4565a652026a4db2bcdf68f2fb589ea80d0be70e03929ed730746b84fe", size = 166393 },
]
`

const (
	m13AliyunPrefix   = "https://mirrors.aliyun.com/pypi/"
	m13TsinghuaPrefix = "https://pypi.tuna.tsinghua.edu.cn/"
	m13USTCPrefix     = "https://pypi.mirrors.ustc.edu.cn/"
	m13OfficialIndex  = "https://pypi.org/simple"
)

// TestM13Component_DependencySyncMirrorRotation 用真实 uv 服务 + 假 uv 覆盖 C10
// 的全部收场：换源、回退原锁、全失败、离线不改写、mirror-only 不回退、
// 显式首选排最前，以及取消。每个子用例都断言临时项目目录不残留、
// repo 内的 uv.lock 与 pyproject.toml 字节不变。
func TestM13Component_DependencySyncMirrorRotation(t *testing.T) {
	failFrozen := m13Rule{
		"argumentsContain": []string{"--frozen"},
		"exitCode":         1,
		"stderr":           []string{"mirror is unreachable"},
	}
	tests := []struct {
		name           string
		rules          []m13Rule
		arguments      []string
		wantCode       protocol.Code
		wantSource     string
		wantRewritten  bool
		wantAttempts   float64
		wantLockPrefix string
	}{
		{
			name: "first mirror fails then the next one succeeds",
			rules: []m13Rule{{
				"argumentsContain": []string{"--frozen"},
				"lockContains":     m13AliyunPrefix,
				"exitCode":         1,
				"stderr":           []string{"aliyun is unreachable"},
			}},
			arguments:      []string{"dependencies", "sync"},
			wantCode:       protocol.CodeOK,
			wantSource:     "tsinghua",
			wantRewritten:  true,
			wantAttempts:   2,
			wantLockPrefix: m13TsinghuaPrefix,
		},
		{
			name:           "all mirrors fail and the original lock succeeds",
			rules:          []m13Rule{failFrozen},
			arguments:      []string{"dependencies", "sync"},
			wantCode:       protocol.CodeOK,
			wantSource:     "pypi",
			wantAttempts:   4,
			wantLockPrefix: m13OfficialIndex,
		},
		{
			name: "every source fails including the original lock",
			rules: []m13Rule{failFrozen, {
				"argumentsContain": []string{"--locked"},
				"exitCode":         1,
				"stderr":           []string{"pypi is unreachable"},
			}},
			arguments: []string{"dependencies", "sync"},
			wantCode:  protocol.CodeDependencySyncFailed,
		},
		{
			name:           "offline never rewrites the lock",
			arguments:      []string{"--offline", "dependencies", "sync"},
			wantCode:       protocol.CodeOK,
			wantSource:     "",
			wantAttempts:   1,
			wantLockPrefix: m13OfficialIndex,
		},
		{
			name:      "mirror only does not fall back to the official source",
			rules:     []m13Rule{failFrozen},
			arguments: []string{"--mirror-only", "dependencies", "sync"},
			wantCode:  protocol.CodeMirrorExhausted,
		},
		{
			name:           "an explicit preference is attempted first",
			arguments:      []string{"--mirror", "package-index=ustc", "dependencies", "sync"},
			wantCode:       protocol.CodeOK,
			wantSource:     "ustc",
			wantRewritten:  true,
			wantAttempts:   1,
			wantLockPrefix: m13USTCPrefix,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newM13ComponentFixture(t)
			fixture.reconfigure(t, test.rules)

			stdout, code := fixture.run(t, strings.NewReader(""), test.arguments...)
			result := m13ResultEvent(t, stdout)
			if got := eventString(result, "code"); got != string(test.wantCode) {
				t.Fatalf("result code = %q, want %q; stdout=%q", got, test.wantCode, stdout)
			}
			wantExit := m13ExitCodeFor(t, test.wantCode)
			if code != wantExit {
				t.Fatalf("exit code = %d, want %d; stdout=%q", code, wantExit, stdout)
			}
			if test.wantCode == protocol.CodeOK {
				details, ok := result.object["details"].(map[string]any)
				if !ok {
					t.Fatalf("result details = %#v, want object", result.object["details"])
				}
				wants := map[string]any{
					"sourceKind":    "package-index",
					"source":        test.wantSource,
					"attemptCount":  test.wantAttempts,
					"lockRewritten": test.wantRewritten,
				}
				for key, want := range wants {
					if got := details[key]; got != want {
						t.Errorf("result details[%q] = %#v, want %#v", key, got, want)
					}
				}
			}
			if test.wantLockPrefix != "" {
				fixture.assertLastSyncUsedPrefix(t, test.wantLockPrefix)
			}
			if test.wantCode == protocol.CodeMirrorExhausted {
				fixture.assertNeverUsedPrefix(t, m13OfficialIndex)
			}
			fixture.assertRepositoryUntouched(t)
			fixture.assertNoStagingLeftovers(t)
		})
	}
}

// TestM13Component_DependencySyncCancellationRemovesStagingProject 证明取消发生在
// 一次镜像尝试进行中时，临时项目目录仍然被收口。假 uv 先写 ready 文件再等 release，
// 测试确认 ready 出现后才把 cancel 写进 stdin，因此取消一定落在尝试中。
func TestM13Component_DependencySyncCancellationRemovesStagingProject(t *testing.T) {
	fixture := newM13ComponentFixture(t)
	signals := t.TempDir()
	readyPath := filepath.Join(signals, "ready")
	releasePath := filepath.Join(signals, "release")
	fixture.reconfigure(t, []m13Rule{{
		"argumentsContain": []string{"--frozen"},
		"readyFile":        readyPath,
		"releaseFile":      releasePath,
		"exitCode":         1,
	}})

	reader, writer := io.Pipe()
	defer func() {
		if err := writer.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("close stdin writer: %v", err)
		}
	}()
	go func() {
		if err := m13WaitForFile(readyPath); err != nil {
			return
		}
		_, _ = io.WriteString(
			writer,
			`{"protocol":1,"command":"cancel","commandId":"01J00000000000000000000099"}`+"\n",
		)
		// 兜底：正常路径下取消会 kill 假 uv；万一没 kill 成功，
		// release 文件让它自行退出，测试不至于挂死。
		time.AfterFunc(5*time.Second, func() {
			_ = os.WriteFile(releasePath, []byte("go\n"), 0o600)
		})
	}()

	stdout, code := fixture.run(t, reader, "dependencies", "sync")
	if code != protocol.ExitCodeOperationCancelled {
		t.Fatalf("exit code = %d, want %d; stdout=%q", code, protocol.ExitCodeOperationCancelled, stdout)
	}
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatalf("ready file stat error = %v, want the mirror attempt to have started", err)
	}
	fixture.assertRepositoryUntouched(t)
	fixture.assertNoStagingLeftovers(t)
}

type m13Rule map[string]any

type m13ComponentFixture struct {
	appRoot    string
	layout     *config.Layout
	options    []Option
	configPath string
	recordPath string
	snapshot   map[string]string
}

// newM13ComponentFixture 先用「全部成功」的假 uv 跑一次 bootstrap，
// 让受管环境进入 ready_to_start，随后的 dependencies sync 才是被测对象。
func newM13ComponentFixture(t *testing.T) *m13ComponentFixture {
	t.Helper()
	appRoot := t.TempDir()
	layout, err := config.NewLayout(appRoot, appRoot)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	gitFixture := newM5ComponentGitFixture(t)
	fakeUV := buildM5ComponentFakeUV(t)
	workspace := t.TempDir()
	fixture := &m13ComponentFixture{
		appRoot:    appRoot,
		layout:     layout,
		configPath: filepath.Join(workspace, "fake-uv-config.json"),
		recordPath: filepath.Join(workspace, "fake-uv-record.jsonl"),
	}
	t.Setenv("FAKE_UV_CONFIG", fixture.configPath)
	t.Setenv("FAKE_UV_RECORD", fixture.recordPath)
	fixture.writeConfig(t, nil)
	fixture.options = m5ComponentOptions(t, appRoot, gitFixture, fakeUV)
	runM5ComponentCommand(t, appRoot, fixture.options, "bootstrap", "--version", gitFixture.version)
	fixture.snapshot = map[string]string{
		layout.UVLockFile():    m13FileDigest(t, layout.UVLockFile()),
		layout.PyProjectFile(): m13FileDigest(t, layout.PyProjectFile()),
	}
	return fixture
}

// reconfigure 换上本用例的失败注入规则，并丢弃 bootstrap 阶段的调用记录。
func (f *m13ComponentFixture) reconfigure(t *testing.T, rules []m13Rule) {
	t.Helper()
	f.writeConfig(t, rules)
	if err := os.Remove(f.recordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove fake uv record: %v", err)
	}
}

func (f *m13ComponentFixture) writeConfig(t *testing.T, extra []m13Rule) {
	t.Helper()
	pythonExecutable := filepath.Join(f.layout.PythonDir(), "cpython-3.12.10", "python.exe")
	rules := []m13Rule{
		{"argumentsPrefix": []string{"--version"}, "stdout": []string{"uv " + uv.FixedVersion}},
		{"argumentsPrefix": []string{"python", "list"}, "stdout": []string{`[{"version":"3.12.10"}]`}},
		{"argumentsPrefix": []string{"python", "install"}, "createDirectories": []string{filepath.Dir(pythonExecutable)}},
		{"argumentsPrefix": []string{"python", "find"}, "stdout": []string{pythonExecutable}},
		{"argumentsPrefix": []string{"lock"}},
	}
	rules = append(rules, extra...)
	rules = append(rules, m13Rule{
		"argumentsPrefix":   []string{"sync"},
		"createDirectories": []string{f.layout.VenvDir()},
	})
	payload, err := json.Marshal(map[string]any{"exitCode": 99, "rules": rules})
	if err != nil {
		t.Fatalf("json.Marshal(fake uv config) error = %v", err)
	}
	if err := os.WriteFile(f.configPath, payload, 0o600); err != nil {
		t.Fatalf("WriteFile(fake uv config) error = %v", err)
	}
}

func (f *m13ComponentFixture) run(t *testing.T, stdin io.Reader, arguments ...string) (string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	args := append([]string{"--app-root", f.appRoot, "--output", "ndjson"}, arguments...)
	code := Execute(
		t.Context(),
		args,
		IO{In: stdin, Out: &stdout, Err: &stderr},
		f.options...,
	)
	return stdout.String(), code
}

func (f *m13ComponentFixture) assertRepositoryUntouched(t *testing.T) {
	t.Helper()
	for path, want := range f.snapshot {
		if got := m13FileDigest(t, path); got != want {
			t.Errorf("%q digest = %s, want %s (repository files must stay byte-identical)", path, got, want)
		}
	}
}

func (f *m13ComponentFixture) assertNoStagingLeftovers(t *testing.T) {
	t.Helper()
	root := filepath.Join(f.layout.BuildCacheDir(), "dependencies")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("ReadDir(%q) error = %v", root, err)
	}
	if len(entries) == 0 {
		return
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	t.Errorf("dependency staging leftovers in %q: %v", root, names)
}

// assertLastSyncUsedPrefix 断言最后一次 uv sync 看到的锁确实指向该前缀。
func (f *m13ComponentFixture) assertLastSyncUsedPrefix(t *testing.T, prefix string) {
	t.Helper()
	invocations := f.syncInvocations(t)
	if len(invocations) == 0 {
		t.Fatal("no uv sync invocation was recorded")
	}
	last := invocations[len(invocations)-1]
	for _, got := range last.LockIndexPrefixes {
		if strings.HasPrefix(got, prefix) {
			return
		}
	}
	t.Errorf("last sync lock prefixes = %v, want one starting with %q", last.LockIndexPrefixes, prefix)
}

func (f *m13ComponentFixture) assertNeverUsedPrefix(t *testing.T, prefix string) {
	t.Helper()
	for _, invocation := range f.syncInvocations(t) {
		for _, got := range invocation.LockIndexPrefixes {
			if strings.HasPrefix(got, prefix) {
				t.Errorf("a uv sync used forbidden prefix %q (arguments=%v)", prefix, invocation.Arguments)
			}
		}
	}
}

type m13Invocation struct {
	Arguments         []string `json:"arguments"`
	ProjectDir        string   `json:"projectDir"`
	LockIndexPrefixes []string `json:"lockIndexPrefixes"`
}

func (f *m13ComponentFixture) syncInvocations(t *testing.T) []m13Invocation {
	t.Helper()
	file, err := os.Open(f.recordPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("os.Open(fake uv record) error = %v", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			t.Errorf("fake uv record Close() error = %v", closeErr)
		}
	}()
	invocations := make([]m13Invocation, 0, 8)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var invocation m13Invocation
		if err := json.Unmarshal(scanner.Bytes(), &invocation); err != nil {
			t.Fatalf("decode fake uv record: %v", err)
		}
		if len(invocation.Arguments) > 0 && invocation.Arguments[0] == "sync" {
			invocations = append(invocations, invocation)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan fake uv record: %v", err)
	}
	return invocations
}

func m13ResultEvent(t *testing.T, output string) parsedEvent {
	t.Helper()
	events := parseNDJSON(t, output)
	for _, event := range events {
		if eventType(event) == string(protocol.TypeResult) {
			return event
		}
	}
	t.Fatalf("result event is missing from %q", output)
	return parsedEvent{}
}

func m13ExitCodeFor(t *testing.T, code protocol.Code) int {
	t.Helper()
	if code == protocol.CodeOK {
		return protocol.ExitCodeSuccess
	}
	definition, ok := protocol.LookupErrorDefinition(code)
	if !ok {
		t.Fatalf("error definition for %q is missing", code)
	}
	return definition.ExitCode
}

func m13FileDigest(t *testing.T, path string) string {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

// m13WaitForFile 轮询等待信号文件出现，超时即放弃（调用方据此不再注入取消）。
func m13WaitForFile(path string) error {
	deadline, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-deadline.Done():
			return deadline.Err()
		case <-ticker.C:
		}
	}
}
