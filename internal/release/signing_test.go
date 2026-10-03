package release

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("got no caller path, want test source path")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func TestReleaseWorkflow_SigningContract(t *testing.T) {
	source := releaseWorkflowSource(t)
	for _, want := range []string{
		"scripts/request-runtime-signing.ps1",
		"AUTO_MAS_SIGNING_TOKEN: ${{ secrets.AUTO_MAS_SIGNING_TOKEN }}",
		"repository: AUTO-MAS-Project/AUTO-MAS",
		"artifact-ids: ${{ steps.signing.outputs.artifact_id }}",
		"run-id: ${{ steps.signing.outputs.run_id }}",
		"RUNTIME_SIGNING_CERTIFICATE_THUMBPRINT",
		"scripts/verify-runtime-release.ps1",
		"Windows x64 build. The executable is Authenticode signed.",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("got workflow without %q, want signing gate", want)
		}
	}
	if strings.Contains(source, "intentionally unsigned") || strings.Contains(source, "go build -trimpath") {
		t.Fatal("got unsigned local build path, want main-repository signed artifact only")
	}
}

func TestSigningRequest_EndToEnd(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("signing request fixtures require PowerShell 7")
	}
	for _, test := range []struct {
		name      string
		fault     string
		wantError string
	}{
		{name: "successful exact request"},
		{name: "unrelated concurrent request", fault: "unrelated"},
		{name: "run appears after dispatch", fault: "delayed"},
		{name: "wait for running signing job", fault: "running"},
		{name: "main moved", fault: "sha", wantError: "origin does not match"},
		{name: "wrong repository", fault: "repo", wantError: "origin does not match"},
		{name: "wrong branch", fault: "branch", wantError: "origin does not match"},
		{name: "wrong workflow", fault: "path", wantError: "origin does not match"},
		{name: "wrong event", fault: "event", wantError: "origin does not match"},
		{name: "signing failed", fault: "failure", wantError: "did not succeed"},
		{name: "duplicate request", fault: "duplicate", wantError: "multiple signing runs"},
		{name: "expired artifact", fault: "expired", wantError: "artifact identity"},
		{name: "wrong artifact source", fault: "artifact", wantError: "artifact identity"},
		{name: "no signing response", fault: "timeout", wantError: "timed out"},
		{name: "API failure hides credentials", fault: "auth", wantError: "HTTP 401"},
	} {
		t.Run(test.name, func(t *testing.T) {
			polls := 0
			dispatched := false
			run := map[string]any{
				"id": 42, "run_attempt": 1, "display_title": "Runtime signing 123-1",
				"repository": map[string]any{"full_name": "AUTO-MAS-Project/AUTO-MAS"},
				"event":      "workflow_dispatch", "path": ".github/workflows/build-sign-runtime.yml",
				"head_branch": "main", "head_sha": testCommit, "status": "completed", "conclusion": "success",
			}
			switch test.fault {
			case "sha":
				run["head_sha"] = strings.Repeat("f", 40)
			case "repo":
				run["repository"] = map[string]any{"full_name": "attacker/repository"}
			case "branch":
				run["head_branch"] = "dev"
			case "path":
				run["path"] = ".github/workflows/other.yml"
			case "event":
				run["event"] = "pull_request"
			case "failure":
				run["conclusion"] = "failure"
			case "running":
				run["status"] = "in_progress"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+testToken {
					t.Errorf("got unexpected Authorization header, want fixture token")
				}
				w.Header().Set("Content-Type", "application/json")
				path := strings.TrimPrefix(r.URL.Path, "/repos/AUTO-MAS-Project/AUTO-MAS/")
				var response any
				switch path {
				case "git/ref/heads/main":
					if test.fault == "auth" {
						w.WriteHeader(http.StatusUnauthorized)
						fmt.Fprint(w, testToken)
						return
					}
					response = map[string]any{"object": map[string]any{"type": "commit", "sha": testCommit}}
				case "actions/workflows/build-sign-runtime.yml/dispatches":
					var request struct {
						Ref    string            `json:"ref"`
						Inputs map[string]string `json:"inputs"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Errorf("got invalid dispatch JSON: %v", err)
					}
					if r.Method != http.MethodPost || request.Ref != "main" || request.Inputs["runtime_commit"] != testCommit || request.Inputs["request_id"] != "123-1" || request.Inputs["runtime_tag"] != "v0.1.0" || request.Inputs["build_date"] != "2026-10-03T00:00:00Z" {
						t.Errorf("got dispatch %+v, want exact source request", request)
					}
					dispatched = true
					w.WriteHeader(http.StatusNoContent)
					return
				case "actions/workflows/build-sign-runtime.yml/runs":
					polls++
					if r.URL.Query().Get("branch") != "main" || r.URL.Query().Get("event") != "workflow_dispatch" {
						t.Error("got unfiltered run lookup, want main dispatch filter")
					}
					items := []any{run}
					if test.fault == "timeout" || (test.fault == "delayed" && polls == 1) {
						items = []any{}
					}
					if test.fault == "duplicate" {
						items = append(items, run)
					}
					if test.fault == "unrelated" {
						items = append(items, map[string]any{"display_title": "Runtime signing 999-1"})
					}
					response = map[string]any{"workflow_runs": items}
				case "actions/runs/42":
					run["status"] = "completed"
					response = run
				case "actions/runs/42/artifacts":
					artifactSHA := testCommit
					if test.fault == "artifact" {
						artifactSHA = strings.Repeat("f", 40)
					}
					response = map[string]any{"artifacts": []any{map[string]any{
						"id": 84, "name": "runtime-signed-123-1", "expired": test.fault == "expired",
						"workflow_run": map[string]any{"id": 42, "head_sha": artifactSHA},
					}}}
				default:
					t.Errorf("got unexpected API request %s", r.URL)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Errorf("write fixture response: %v", err)
				}
			}))
			defer server.Close()
			outputPath := filepath.Join(t.TempDir(), "outputs.txt")
			cmd := exec.CommandContext(t.Context(), "pwsh", "-NoProfile", "-NonInteractive", "-File",
				filepath.Join(repositoryRoot(t), "scripts", "request-runtime-signing.ps1"),
				"-Tag", "v0.1.0", "-SourceCommit", testCommit, "-RequestID", "123-1",
				"-BuildDate", "2026-10-03T00:00:00Z", "-OutputPath", outputPath, "-ApiUrl", server.URL,
				"-PollSeconds", "1", "-TimeoutSeconds", "3")
			cmd.Env = signingTestEnv("AUTO_MAS_SIGNING_TOKEN", testToken)
			output, err := cmd.CombinedOutput()
			server.Close()
			if strings.Contains(string(output), testToken) {
				t.Fatal("got credentials in signing output, want sanitized errors")
			}
			if test.wantError != "" {
				if err == nil || !strings.Contains(string(output), test.wantError) {
					t.Fatalf("got error %v and output %s, want %q", err, output, test.wantError)
				}
				if data, _ := os.ReadFile(outputPath); len(data) != 0 {
					t.Fatalf("got publishable outputs after failure: %s", data)
				}
				return
			}
			if err != nil {
				t.Fatalf("got request error %v: %s, want success", err, output)
			}
			data, err := os.ReadFile(outputPath)
			if err != nil || strings.ReplaceAll(string(data), "\r\n", "\n") != "run_id=42\nrun_attempt=1\nartifact_id=84\n" || !dispatched {
				t.Fatalf("got outputs %q and error %v, want exact signed run and artifact", data, err)
			}
		})
	}
}

func TestRuntimeRelease_MetadataAndUnsignedRejection(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Authenticode gate requires Windows")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "executed.txt")
	fixture := filepath.Join(dir, "fixture.go")
	if err := os.WriteFile(fixture, []byte(`package main
import "os"
func main() { if err := os.WriteFile(os.Getenv("SIGNING_EXECUTION_MARKER"), []byte("executed"), 0600); err != nil { panic(err) } }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "unsigned.exe")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, fixture)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("got fixture build error %v: %s", err, output)
	}
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "unsigned rejected before execution", args: []string{"-RequireSignature", "-CertificateThumbprint", strings.Repeat("a", 40)}, want: "Authenticode signature"},
		{name: "certificate configuration required", args: []string{"-RequireSignature"}, want: "CERTIFICATE_THUMBPRINT"},
		{name: "incorrect Windows metadata rejected", want: "Windows version metadata"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{"-NoProfile", "-NonInteractive", "-File", filepath.Join(repositoryRoot(t), "scripts", "verify-runtime-release.ps1"),
				"-BinaryPath", binary, "-Tag", "v0.1.0", "-SourceCommit", testCommit, "-BuildDate", "2026-10-03T00:00:00Z"}
			cmd := exec.CommandContext(t.Context(), "pwsh", append(args, test.args...)...)
			cmd.Env = signingTestEnv("SIGNING_EXECUTION_MARKER", marker)
			output, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(output), test.want) {
				t.Fatalf("got error %v and output %s, want %q", err, output, test.want)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("got executable side effect with error %v, want rejection before execution", err)
			}
		})
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	for _, test := range []struct {
		name  string
		field string
		value any
		want  string
	}{
		{name: "ISO date preserved", want: "Windows version metadata"},
		{name: "wrong manifest commit", field: "commit", value: strings.Repeat("f", 40), want: "signing manifest"},
		{name: "wrong manifest tag", field: "tag", value: "v0.1.1", want: "signing manifest"},
		{name: "wrong manifest date", field: "buildDate", value: "2026-10-04T00:00:00Z", want: "signing manifest"},
		{name: "wrong manifest hash", field: "sha256", value: strings.Repeat("f", 64), want: "signing manifest"},
		{name: "wrong manifest request", field: "requestID", value: "124-1", want: "signing manifest"},
		{name: "wrong signing run", field: "signingRunID", value: "43", want: "signing manifest"},
		{name: "wrong signing attempt", field: "signingRunAttempt", value: "2", want: "signing manifest"},
		{name: "wrong manifest filename", field: "binaryName", value: "other.exe", want: "signing manifest"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := map[string]any{
				"schema": 1, "tag": "v0.1.0", "commit": testCommit, "buildDate": "2026-10-03T00:00:00Z",
				"requestID": "123-1", "signingRunID": "42", "signingRunAttempt": "1", "binaryName": "unsigned.exe", "sha256": hash,
			}
			if test.field != "" {
				manifest[test.field] = test.value
			}
			manifestPath := filepath.Join(t.TempDir(), "manifest.json")
			content, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, content, 0o600); err != nil {
				t.Fatal(err)
			}
			output, err := runPwsh(t, filepath.Join(repositoryRoot(t), "scripts", "verify-runtime-release.ps1"),
				"-BinaryPath", binary, "-Tag", "v0.1.0", "-SourceCommit", testCommit, "-BuildDate", "2026-10-03T00:00:00Z",
				"-ManifestPath", manifestPath, "-RequestID", "123-1", "-SigningRunID", "42", "-SigningRunAttempt", "1")
			if err == nil || !strings.Contains(output, test.want) {
				t.Fatalf("got error %v and output %s, want %q", err, output, test.want)
			}
		})
	}
}

func signingTestEnv(key, value string) []string {
	env := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), strings.ToUpper(key)+"=") {
			env = append(env, entry)
		}
	}
	return append(env, key+"="+value)
}
