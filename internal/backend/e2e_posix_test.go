//go:build linux || darwin

package backend

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/health"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

func TestPOSIXBackend_RealRuntimeLifecycle(t *testing.T) {
	binaries := t.TempDir()
	runtimeBinary := buildPOSIXFixture(t, binaries, "runtime", "../../cmd/auto-mas-runtime")
	uvBinary := buildPOSIXFixture(t, binaries, "uv", "../../testdata/fakeuv")
	backendBinary := buildPOSIXFixture(t, binaries, "backend", "../../testdata/fakebackend")
	for _, tc := range []struct {
		name     string
		force    bool
		explicit bool
		managed  bool
		restart  bool
	}{{name: "stdin EOF"}, {name: "shutdown control", explicit: true}, {name: "shutdown timeout", force: true}, {name: "managed stdin EOF", managed: true}, {name: "single restart", restart: true}} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			app := filepath.Join(root, "app")
			repo := filepath.Join(root, "repo")
			if err = os.MkdirAll(app, 0700); err != nil {
				t.Fatal(err)
			}
			layout, err := config.NewLayout(app, root)
			if err != nil {
				t.Fatal(err)
			}
			if tc.managed {
				repo = layout.RepoDir()
			}
			uvPath, err := layout.UVExecutable(uv.FixedVersion)
			if err != nil {
				t.Fatal(err)
			}
			python := config.PythonExecutableInVenv(filepath.Join(repo, ".venv"))
			if tc.managed {
				python = layout.VenvPythonExecutable()
			}
			for target, source := range map[string]string{uvPath: uvBinary, python: backendBinary} {
				if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(source)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(target, data, 0700); err != nil {
					t.Fatal(err)
				}
			}
			for name, data := range map[string]string{"main.py": "# fake backend\n", "pyproject.toml": "[project]\nname='fixture'\nversion='1.0.0'\n", ".venv/pyvenv.cfg": "home = managed\n"} {
				if err = os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(repo, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			port := pickPOSIXBackendPort(t)
			shutdown := filepath.Join(root, "shutdown")
			backendConfig := map[string]any{"shutdownFile": shutdown, "spawnGrandchild": true, "grandchildLifetimeMs": 60000, "health": []any{map[string]any{"ready": true, "backgroundStatus": "ready", "protocol": 1}}}
			if tc.force {
				backendConfig["shutdownDelayMs"] = 10000
			}
			if tc.restart {
				backendConfig["crashAfterHealthRequests"] = 3
				backendConfig["crashExitCode"] = 73
			}
			if tc.managed {
				commit := seedPOSIXManagedRepository(t, layout)
				backendConfig["health"] = []any{map[string]any{"ready": true, "backgroundStatus": "ready", "protocol": 1, "version": "v1.0.0", "commit": commit}}
			}
			backendConfigPath := writePOSIXJSON(t, root, "backend.json", backendConfig)
			uvConfigPath := writePOSIXJSON(t, root, "uv.json", map[string]any{"rules": []any{map[string]any{"argumentsPrefix": []string{"--version"}, "stdout": []string{"uv " + uv.FixedVersion}}, map[string]any{"argumentsPrefix": []string{"run"}, "exec": python}}})
			args := []string{"--app-root", app, "--output", "ndjson", "backend", "supervise", "--mode", "development", "--repo", repo, "--port", strconv.Itoa(port), "--shutdown-timeout", "1"}
			if tc.managed {
				args = []string{"--app-root", app, "--output", "ndjson", "backend", "supervise", "--mode", "managed", "--port", strconv.Itoa(port), "--shutdown-timeout", "1"}
			}
			command := exec.CommandContext(t.Context(), runtimeBinary, args...)
			command.Env = append(os.Environ(), "FAKE_UV_CONFIG="+uvConfigPath, "FAKE_BACKEND_CONFIG="+backendConfigPath, "AUTO_MAS_TELEMETRY=disabled")
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			command.Stderr = &stderr
			if err = command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			events := make(chan map[string]any, 128)
			go func() {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					var event map[string]any
					if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
						events <- map[string]any{"type": "invalid"}
						continue
					}
					events <- event
				}
				close(events)
			}()
			go func() { done <- command.Wait() }()
			t.Cleanup(func() { _ = stdin.Close(); _ = command.Process.Kill() })
			timeout := time.NewTimer(20 * time.Second)
			defer timeout.Stop()
			running := false
			runningCount := 0
			results := 0
			forced := false
			for {
				select {
				case event, ok := <-events:
					if !ok {
						if !running || results != 1 || tc.restart && runningCount != 2 {
							t.Fatalf("got running %v results %d stderr %s", running, results, stderr.String())
						}
						if tc.force && !forced {
							t.Fatal("missing forced termination warning")
						}
						select {
						case err := <-done:
							if err != nil {
								t.Fatal(err)
							}
						case <-timeout.C:
							t.Fatal("runtime wait timed out")
						}
						if !tc.force {
							if data, err := os.ReadFile(shutdown); err != nil || strings.TrimSpace(string(data)) != "graceful" {
								t.Fatalf("shutdown marker %q %v", data, err)
							}
						}
						return
					}
					if event["type"] == "invalid" {
						t.Fatal("non-NDJSON stdout")
					}
					if event["type"] == "state" && event["status"] == "running" {
						running = true
						runningCount++
						if tc.restart && runningCount == 1 {
							req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, health.HealthURLForPort(port), nil)
							if err != nil {
								t.Fatal(err)
							}
							response, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
							if err != nil {
								t.Fatal(err)
							}
							if err = response.Body.Close(); err != nil {
								t.Fatal(err)
							}
							continue
						}
						if tc.explicit {
							_, err = fmt.Fprintln(stdin, `{"protocol":1,"command":"shutdown","commandId":"01ARZ3NDEKTSV4RRFFQ69G5FAV"}`)
						} else {
							err = stdin.Close()
						}
						if err != nil {
							t.Fatal(err)
						}
					}
					if event["type"] == "warning" && event["code"] == "BACKEND_FORCE_TERMINATED" {
						forced = true
					}
					if event["type"] == "result" {
						results++
						if event["success"] != true {
							t.Fatalf("failed result: %+v; stderr %s", event, stderr.String())
						}
					}
				case <-timeout.C:
					t.Fatalf("lifecycle timeout; running=%v stderr=%s", running, stderr.String())
				}
			}
		})
	}
}
func buildPOSIXFixture(t *testing.T, root, name, source string) string {
	t.Helper()
	path := filepath.Join(root, name)
	command := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=false", "-o", path, source)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, output)
	}
	return path
}
func writePOSIXJSON(t *testing.T, root, name string, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, name)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func seedPOSIXManagedRepository(t *testing.T, layout *config.Layout) string {
	t.Helper()
	repo, err := git.PlainInit(layout.RepoDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	version := layout.RepoVersionFile()
	if err = os.MkdirAll(filepath.Dir(version), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(version, []byte(`{"version":"v1.0.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"main.py", "pyproject.toml", "res/version.json"} {
		if _, err = worktree.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	hash, err := worktree.Commit("fixture", &git.CommitOptions{Author: &object.Signature{Name: "fixture", Email: "test@example.invalid", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	branch := plumbing.NewBranchReferenceName("release/v1.0.0")
	if err = repo.Storer.SetReference(plumbing.NewHashReference(branch, hash)); err != nil {
		t.Fatal(err)
	}
	if err = repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branch)); err != nil {
		t.Fatal(err)
	}
	if err = repo.Storer.SetShallow([]plumbing.Hash{hash}); err != nil {
		t.Fatal(err)
	}
	catalog, err := mirror.DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{catalog.Sources(mirror.KindGit)[0].BaseURL()}}); err != nil {
		t.Fatal(err)
	}
	store, err := state.NewStore(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := store.NewReadyEnvironment("v1.0.0", hash.String())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.WriteEnvironment(t.Context(), ready); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	return hash.String()
}

func pickPOSIXBackendPort(t *testing.T) int {
	t.Helper()
	// 避开系统自动分配的高位端口，防止随后启动的回环中继抢占刚释放的探测端口。
	for i := 0; i < 100; i++ {
		port := 20000 + rand.IntN(10000)
		listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		if err = listener.Close(); err != nil {
			t.Fatal(err)
		}
		return port
	}
	t.Fatal("no available backend test port")
	return 0
}
