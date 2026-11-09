package uv

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/relay"
)

// TestDependencies_BuildDependenciesIntegration 是显式启用的真实 uv / Python 构建测试。
// 工具由调用者预先准备，测试本身只访问本地 TLS 镜像，禁止自动下载解释器。
func TestDependencies_BuildDependenciesIntegration(t *testing.T) {
	uvExe, pythonDir := os.Getenv("AUTO_MAS_TEST_UV_EXE"), os.Getenv("AUTO_MAS_TEST_PYTHON_DIR")
	if uvExe == "" || pythonDir == "" {
		t.Skip("set AUTO_MAS_TEST_UV_EXE and AUTO_MAS_TEST_PYTHON_DIR to enable real build integration")
	}
	for _, test := range []struct {
		name  string
		relay bool
	}{
		{name: "relay", relay: true},
		{name: "relay start failure rotates mirrors"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMirrorSyncFixture(t)
			files := buildDependencyArtifacts(t)
			var backendIndexes, helperIndexes, backendArtifacts, helperArtifacts, rejected atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rejected.Add(1)
				http.Error(w, "external access blocked", http.StatusForbidden)
			}))
			t.Cleanup(proxy.Close)
			t.Setenv("HTTP_PROXY", proxy.URL)
			t.Setenv("HTTPS_PROXY", proxy.URL)
			t.Setenv("ALL_PROXY", proxy.URL)
			t.Setenv("NO_PROXY", "")
			t.Setenv("UV_DEFAULT_INDEX", "https://untrusted.invalid/simple")
			var server *httptest.Server
			server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 3)
				if len(parts) != 3 {
					http.NotFound(w, r)
					return
				}
				if parts[1] == "simple" {
					if parts[0] == "first" {
						http.Error(w, "build dependency index unavailable", http.StatusNotFound)
						return
					}
					name := strings.TrimSuffix(parts[2], "/")
					switch name {
					case "runtime-test-backend":
						backendIndexes.Add(1)
					case "runtime-test-wheel":
						helperIndexes.Add(1)
					default:
						http.NotFound(w, r)
						return
					}
					filename := strings.ReplaceAll(name, "-", "_") + "-1.0-py3-none-any.whl"
					page := fmt.Sprintf(`<a href="%s/%s/packages/%s#sha256=%x">%s</a>`,
						server.URL, parts[0], filename, sha256.Sum256(files[filename]), filename)
					w.Header().Set("Content-Type", "text/html")
					w.Header().Set("Content-Length", itoa(len(page)))
					_, _ = io.WriteString(w, page)
					return
				}
				content, ok := files[parts[2]]
				if parts[1] != "packages" || !ok {
					http.NotFound(w, r)
					return
				}
				if r.Method == http.MethodGet {
					switch parts[2] {
					case "runtime_test_backend-1.0-py3-none-any.whl":
						backendArtifacts.Add(1)
					case "runtime_test_wheel-1.0-py3-none-any.whl":
						helperArtifacts.Add(1)
					}
				}
				w.Header().Set("Content-Length", itoa(len(content)))
				if r.Method != http.MethodHead {
					_, _ = w.Write(content)
				}
			}))
			certificate, rootPEM := buildIntegrationCertificate(t)
			server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
			server.StartTLS()
			t.Cleanup(server.Close)
			certFile := filepath.Join(t.TempDir(), "mirror.pem")
			if err := os.WriteFile(certFile, rootPEM, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SSL_CERT_FILE", certFile)
			project := "[project]\nname = \"auto-mas\"\nversion = \"0.0.1\"\nrequires-python = \"==3.12.*\"\ndependencies = [\"runtime-test-sdist==1.0\"]\n"
			sdist := files["runtime_test_sdist-1.0.tar.gz"]
			lock := fmt.Sprintf(`version = 1
revision = 3
requires-python = "==3.12.*"
[[package]]
name = "auto-mas"
version = "0.0.1"
source = { virtual = "." }
dependencies = [{ name = "runtime-test-sdist" }]
[package.metadata]
requires-dist = [{ name = "runtime-test-sdist", specifier = "==1.0" }]
[[package]]
name = "runtime-test-sdist"
version = "1.0"
source = { registry = "https://pypi.org/simple" }
sdist = { url = "https://files.pythonhosted.org/packages/runtime_test_sdist-1.0.tar.gz", hash = "sha256:%x", size = %d }
`, sha256.Sum256(sdist), len(sdist))
			for path, text := range map[string]string{fixture.layout.PyProjectFile(): project, fixture.layout.UVLockFile(): lock} {
				if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			runner, err := NewRunner(RunnerConfig{
				Executable: uvExe, ProjectDir: fixture.layout.RepoDir(), PythonInstallDir: pythonDir,
				ProjectEnvDir: fixture.layout.VenvDir(), CacheDir: fixture.layout.UVCacheDir(),
			})
			if err != nil {
				t.Fatal(err)
			}
			realRunner := &buildIntegrationRunner{runner: runner, pythonDir: pythonDir}
			catalog := buildIntegrationCatalog(t, server.URL)
			service, err := NewDependenciesService(fixture.layout, realRunner, &fakeTreeRemover{},
				WithDependenciesStagingRemover(managedTreeRemover{layout: fixture.layout}),
				WithDependenciesPlanner(mirror.CatalogPlanFunc(catalog)),
				WithDependenciesRelay(func(ctx context.Context, cfg relay.Config, deps relay.Deps) (relaySession, error) {
					if !test.relay {
						return nil, errors.New("relay unavailable")
					}
					deps.Client = server.Client()
					return relay.Start(ctx, cfg, deps)
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			request := fixture.request(t, mirror.PolicySpec{MirrorOnly: true})
			request.PythonVersion = "3.12.13"
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			if err := runner.CheckVersion(ctx, FixedVersion, protocol.StageUVVerify, nil); err != nil {
				t.Fatal(err)
			}
			result, err := service.Sync(ctx, request)
			if err != nil {
				t.Fatalf("Sync() error = %v, uv stderr = %s", err, realRunner.last.Stderr)
			}
			wantAttempts := 2
			if test.relay {
				wantAttempts = 1
			}
			if result.AttemptCount != wantAttempts || !result.Synchronized {
				t.Errorf("result = %+v, want %d successful attempts", result, wantAttempts)
			}
			if backendIndexes.Load() == 0 || helperIndexes.Load() == 0 || backendArtifacts.Load() == 0 || helperArtifacts.Load() == 0 || rejected.Load() != 0 {
				t.Errorf("requests backend index/artifact=%d/%d helper=%d/%d external=%d, want both build dependencies and no external requests",
					backendIndexes.Load(), backendArtifacts.Load(), helperIndexes.Load(), helperArtifacts.Load(), rejected.Load())
			}
			installed := filepath.Join(fixture.layout.VenvDir(), "Lib", "site-packages", "runtime_test_sdist.py")
			if _, err := os.Stat(installed); err != nil {
				t.Errorf("built package Stat() error = %v", err)
			}
			for path, want := range map[string]string{fixture.layout.PyProjectFile(): project, fixture.layout.UVLockFile(): lock} {
				if got, err := os.ReadFile(path); err != nil || string(got) != want {
					t.Errorf("repository input changed: %s, error = %v", path, err)
				}
			}
			fixture.assertNoStagingLeftovers(t)
		})
	}
}

func buildIntegrationCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{
		SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		NotBefore: root.NotBefore, NotAfter: root.NotAfter,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{leafDER, rootDER}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
}

type buildIntegrationRunner struct {
	runner    *UVRunner
	pythonDir string
	last      UVResult
}

func (r *buildIntegrationRunner) Run(ctx context.Context, args []string, options RunOptions) (UVResult, error) {
	options.PythonInstallDir = r.pythonDir
	result, err := r.runner.Run(ctx, append(slices.Clone(args), "--no-python-downloads"), options)
	r.last = result
	return result, err
}

func buildIntegrationCatalog(t *testing.T, base string) *mirror.Catalog {
	t.Helper()
	defaults, err := mirror.DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	var sources []mirror.Source
	for _, kind := range mirror.AllKinds() {
		if kind != mirror.KindPackageIndex {
			sources = append(sources, defaults.Sources(kind)...)
		}
	}
	for _, key := range []string{"first", "second"} {
		source, err := mirror.NewPackageIndexSource(mirror.PackageIndexSpec{
			Key: key, BaseURL: base + "/" + key + "/simple/", SimpleBase: base + "/" + key + "/simple", PackagesBase: base + "/" + key + "/packages/",
		})
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, source)
	}
	for _, source := range defaults.Sources(mirror.KindPackageIndex) {
		if source.Official() {
			sources = append(sources, source)
		}
	}
	catalog, err := mirror.NewCatalog(sources)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func buildDependencyArtifacts(t *testing.T) map[string][]byte {
	t.Helper()
	backend := `import os, zipfile
import runtime_test_wheel
def build_wheel(wheel_directory, config_settings=None, metadata_directory=None):
    name = "runtime_test_sdist-1.0-py3-none-any.whl"
    with zipfile.ZipFile(os.path.join(wheel_directory, name), "w") as wheel:
        wheel.writestr("runtime_test_sdist.py", "built = True\n")
        wheel.writestr("runtime_test_sdist-1.0.dist-info/METADATA", "Metadata-Version: 2.1\nName: runtime-test-sdist\nVersion: 1.0\n")
        wheel.writestr("runtime_test_sdist-1.0.dist-info/WHEEL", "Wheel-Version: 1.0\nGenerator: fixture\nRoot-Is-Purelib: true\nTag: py3-none-any\n")
        wheel.writestr("runtime_test_sdist-1.0.dist-info/RECORD", "")
    return name
`
	files := map[string][]byte{}
	for module, source := range map[string]string{"runtime_test_backend": backend, "runtime_test_wheel": "fixture = True\n"} {
		var buffer bytes.Buffer
		writer := zip.NewWriter(&buffer)
		for path, text := range map[string]string{
			module + ".py":                     source,
			module + "-1.0.dist-info/METADATA": "Metadata-Version: 2.1\nName: " + strings.ReplaceAll(module, "_", "-") + "\nVersion: 1.0\n",
			module + "-1.0.dist-info/WHEEL":    "Wheel-Version: 1.0\nGenerator: fixture\nRoot-Is-Purelib: true\nTag: py3-none-any\n",
			module + "-1.0.dist-info/RECORD":   "",
		} {
			entry, err := writer.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(entry, text); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		files[module+"-1.0-py3-none-any.whl"] = buffer.Bytes()
	}
	project := "[build-system]\nrequires = [\"runtime-test-backend==1.0\", \"runtime-test-wheel==1.0\"]\nbuild-backend = \"runtime_test_backend\"\n[project]\nname = \"runtime-test-sdist\"\nversion = \"1.0\"\n"
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "runtime_test_sdist-1.0/pyproject.toml", Mode: 0o600, Size: int64(len(project))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tarWriter, project); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	files["runtime_test_sdist-1.0.tar.gz"] = buffer.Bytes()
	return files
}
