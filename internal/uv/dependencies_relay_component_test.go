package uv

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/relay"
)

// TestDependencies_RelayComponent 用真实中继串起整条链：锁副本改写到回环地址 → 假 uv 按改写后的 URL 向中继取 wheel
// → 中继从 TLS 假镜像多源取回并校验 sha256 → 交付给 uv；第一台镜像返回坏字节时中继换第二台，
// 摘要把字节记在真正提供文件的源上。
func TestDependencies_RelayComponent(t *testing.T) {
	content := []byte(strings.Repeat("certifi wheel bytes ", 4096))
	digest := sha256.Sum256(content)
	hash := hex.EncodeToString(digest[:])
	const wheelPath = "38/fc/certifi-2025.1.31-py3-none-any.whl"

	var corruptHits, healthyHits atomic.Int32
	corrupt := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/pypi/packages/"+wheelPath {
			http.NotFound(writer, request)
			return
		}
		corruptHits.Add(1)
		bad := append([]byte(nil), content...)
		bad[0] ^= 0xff
		writer.Header().Set("Content-Length", itoa(len(bad)))
		_, _ = writer.Write(bad)
	}))
	defer corrupt.Close()
	healthy := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/pypi/packages/"+wheelPath {
			http.NotFound(writer, request)
			return
		}
		healthyHits.Add(1)
		writer.Header().Set("Content-Length", itoa(len(content)))
		_, _ = writer.Write(content)
	}))
	defer healthy.Close()

	fixture := newMirrorSyncFixture(t)
	lock := strings.Replace(mirrorTestLock, "ca78db4565a652026a4db2bcdf68f2fb589ea80d0be70e03929ed730746b84fe", hash, 1)
	lock = strings.Replace(lock, "size = 166393", "size = "+itoa(len(content)), 1)
	if err := os.WriteFile(fixture.layout.UVLockFile(), []byte(lock), 0o600); err != nil {
		t.Fatalf("WriteFile(uv.lock) error = %v", err)
	}

	catalog := componentCatalog(t, corrupt.URL, healthy.URL)
	roots := x509.NewCertPool()
	roots.AddCert(corrupt.Certificate())
	roots.AddCert(healthy.Certificate())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	trusting := &http.Client{Transport: transport}

	var fetched []byte
	var fetchedURL string
	fixture.runner.onRun = func(args []string, _ RunOptions) {
		if len(args) < 3 || args[0] != "sync" || args[1] != "--project" {
			return
		}
		staged, err := os.ReadFile(filepath.Join(args[2], "uv.lock"))
		if err != nil {
			t.Errorf("ReadFile(staged lock) error = %v", err)
			return
		}
		match := regexp.MustCompile(`http://127\.0\.0\.1:\d+/packages/[^"]+`).FindString(string(staged))
		if match == "" {
			t.Errorf("staged lock has no relay packages url: %q", staged)
			return
		}
		fetchedURL = match
		// 假 uv 的「下载」：像真 uv 一样按锁内 URL 取整个文件。
		response, err := http.Get(fetchedURL)
		if err != nil {
			t.Errorf("GET %s error = %v", fetchedURL, err)
			return
		}
		defer response.Body.Close()
		fetched, _ = io.ReadAll(response.Body)
		if response.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d body = %q", fetchedURL, response.StatusCode, fetched)
		}
	}

	service, err := NewDependenciesService(
		fixture.layout,
		fixture.runner,
		&fakeTreeRemover{},
		WithDependenciesStagingRemover(managedTreeRemover{layout: fixture.layout}),
		WithDependenciesPlanner(mirror.CatalogPlanFunc(catalog)),
		WithDependenciesRelay(func(ctx context.Context, cfg relay.Config, deps relay.Deps) (relaySession, error) {
			deps.Client = trusting
			return relay.Start(ctx, cfg, deps)
		}),
	)
	if err != nil {
		t.Fatalf("NewDependenciesService() error = %v", err)
	}
	fixture.service = service
	result, err := service.Sync(t.Context(), fixture.request(t, mirror.PolicySpec{}))
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if !strings.HasSuffix(fetchedURL, "/packages/"+wheelPath) {
		t.Fatalf("uv fetched %q, want the relay packages route", fetchedURL)
	}
	if got := sha256.Sum256(fetched); hex.EncodeToString(got[:]) != hash {
		t.Fatalf("delivered bytes do not match the lock hash (len %d)", len(fetched))
	}
	if corruptHits.Load() == 0 || healthyHits.Load() != 1 {
		t.Fatalf("mirror hits corrupt=%d healthy=%d, want the relay to fall through to healthy once", corruptHits.Load(), healthyHits.Load())
	}
	if result.Source != "healthy" || result.Relay == nil || result.Relay.Files != 1 || result.Relay.BySource["healthy"] != int64(len(content)) {
		t.Fatalf("result = %+v, want the healthy mirror credited", result)
	}
	if _, err := os.Stat(fixture.layout.RelayStagingDir()); err == nil {
		entries, _ := os.ReadDir(fixture.layout.RelayStagingDir())
		if len(entries) != 0 {
			t.Fatalf("relay staging not emptied: %d entries", len(entries))
		}
	}
	if repoLock, err := os.ReadFile(fixture.layout.UVLockFile()); err != nil || string(repoLock) != lock {
		t.Fatalf("repo/uv.lock changed during relayed sync (err = %v)", err)
	}
	fixture.assertNoStagingLeftovers(t)
}

// componentCatalog 用两台 TLS 假镜像替换包索引目录：corrupt 为普通镜像、healthy 为官方位（保证目录合法）。
func componentCatalog(t *testing.T, corruptURL, healthyURL string) *mirror.Catalog {
	t.Helper()
	defaults, err := mirror.DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	sources := make([]mirror.Source, 0, 12)
	for _, kind := range mirror.AllKinds() {
		if kind == mirror.KindPackageIndex {
			continue
		}
		sources = append(sources, defaults.Sources(kind)...)
	}
	corrupt, err := mirror.NewPackageIndexSource(mirror.PackageIndexSpec{
		Key: "corrupt", BaseURL: corruptURL + "/pypi/simple/", SimpleBase: corruptURL + "/pypi/simple", PackagesBase: corruptURL + "/pypi/packages/",
	})
	if err != nil {
		t.Fatalf("NewPackageIndexSource(corrupt) error = %v", err)
	}
	healthy, err := mirror.NewPackageIndexSource(mirror.PackageIndexSpec{
		Key: "healthy", BaseURL: healthyURL + "/pypi/simple/", SimpleBase: healthyURL + "/pypi/simple", PackagesBase: healthyURL + "/pypi/packages/", Official: true,
	})
	if err != nil {
		t.Fatalf("NewPackageIndexSource(healthy) error = %v", err)
	}
	catalog, err := mirror.NewCatalog(append(sources, corrupt, healthy))
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	return catalog
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
