package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testToken        = "test-cnb-token-0123456789abcdef"
	testUploadSecret = "presigned-upload-secret-do-not-log"
	testCommit       = "0123456789abcdef0123456789abcdef01234567"
)

// cnbTestAsset 记录 CNB 侧已存在的一个资产。
type cnbTestAsset struct {
	Name     string `json:"name"`
	HashAlgo string `json:"hash_algo"`
	Hash     string `json:"hash_value"`
	Size     int64  `json:"size"`
}

type cnbTestRelease struct {
	ID           string         `json:"id"`
	TagName      string         `json:"tag_name"`
	Name         string         `json:"name"`
	Body         string         `json:"body"`
	Draft        bool           `json:"draft"`
	Prerelease   bool           `json:"prerelease"`
	TagCommitish string         `json:"tag_commitish"`
	Assets       []cnbTestAsset `json:"assets"`
}

// cnbTestServer 模拟 CNB OpenAPI 的 Release/Tag/上传子集与公开下载端点，
// 并记录脚本发起的每个请求供断言。
type cnbTestServer struct {
	t          *testing.T
	mu         sync.Mutex
	server     *httptest.Server
	public     *httptest.Server
	releases   map[string]*cnbTestRelease
	tags       map[string]cnbTestTag
	uploads    map[string][]byte
	failAuth   bool
	failUpload bool
	failVerify bool
	requestLog []string
	created    int
}

type cnbTestTag struct {
	Target     string `json:"target"`
	TargetType string `json:"target_type"`
	Commit     struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

func newCnbTestServer(t *testing.T) *cnbTestServer {
	t.Helper()
	c := &cnbTestServer{
		t:        t,
		releases: make(map[string]*cnbTestRelease),
		tags:     make(map[string]cnbTestTag),
		uploads:  make(map[string][]byte),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", c.route)
	c.server = httptest.NewServer(mux)
	publicMux := http.NewServeMux()
	publicMux.HandleFunc("/", c.routePublic)
	c.public = httptest.NewServer(publicMux)
	t.Cleanup(func() {
		c.server.Close()
		c.public.Close()
	})
	return c
}

func (c *cnbTestServer) log(method, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requestLog = append(c.requestLog, method+" "+path)
}

func (c *cnbTestServer) sawPath(needle string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.requestLog {
		if strings.Contains(entry, needle) {
			return true
		}
	}
	return false
}

// mountTagAfter 延迟一段时间后挂载 CNB Tag，模拟 git-sync 尚未同步完成。
func (c *cnbTestServer) mountTagAfter(delay time.Duration, tag cnbTestTag) {
	timer := time.AfterFunc(delay, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.tags["v0.1.0"] = tag
	})
	c.t.Cleanup(func() { timer.Stop() })
}

// authorize 校验 Bearer token；token 错误返回 401。
func (c *cnbTestServer) authorize(w http.ResponseWriter, r *http.Request) bool {
	if c.failAuth {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
		return false
	}
	auth := r.Header.Get("Authorization")
	if auth != "Bearer "+testToken {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
		return false
	}
	return true
}

func (c *cnbTestServer) route(w http.ResponseWriter, r *http.Request) {
	c.log(r.Method, r.URL.Path)
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/-/git/tags/v0.1.0") && r.Method == http.MethodGet:
		if !c.authorize(w, r) {
			return
		}
		c.mu.Lock()
		tag, ok := c.tags["v0.1.0"]
		c.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, tag)
	case strings.HasSuffix(path, "/-/releases/tags/v0.1.0") && r.Method == http.MethodGet:
		if !c.authorize(w, r) {
			return
		}
		c.mu.Lock()
		release, ok := c.releases["v0.1.0"]
		c.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, release)
	case strings.HasSuffix(path, "/-/releases") && r.Method == http.MethodPost:
		if !c.authorize(w, r) {
			return
		}
		var form struct {
			TagName         string `json:"tag_name"`
			Name            string `json:"name"`
			Body            string `json:"body"`
			Draft           bool   `json:"draft"`
			Prerelease      bool   `json:"prerelease"`
			TargetCommitish string `json:"target_commitish"`
		}
		if err := json.NewDecoder(r.Body).Decode(&form); err != nil {
			http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
			return
		}
		if form.TargetCommitish != testCommit {
			http.Error(w, `{"message":"target commitish mismatch"}`, http.StatusUnprocessableEntity)
			return
		}
		c.mu.Lock()
		c.created++
		release := &cnbTestRelease{
			ID:           fmt.Sprintf("release-%d", c.created),
			TagName:      form.TagName,
			Name:         form.Name,
			Body:         form.Body,
			Draft:        form.Draft,
			Prerelease:   form.Prerelease,
			TagCommitish: form.TargetCommitish,
		}
		c.releases["v0.1.0"] = release
		c.mu.Unlock()
		writeJSON(w, release)
	case strings.Contains(path, "/asset-upload-url") && r.Method == http.MethodPost:
		if !c.authorize(w, r) {
			return
		}
		if c.failUpload {
			http.Error(w, `{"message":"storage unavailable"}`, http.StatusInternalServerError)
			return
		}
		var form struct {
			AssetName string `json:"asset_name"`
			Overwrite bool   `json:"overwrite"`
			Size      int64  `json:"size"`
		}
		if err := json.NewDecoder(r.Body).Decode(&form); err != nil {
			http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
			return
		}
		uploadURL := c.server.URL + "/presigned/" + url.PathEscape(form.AssetName) + "?token=" + testUploadSecret
		verifyURL := c.server.URL + "/verify/" + url.PathEscape(form.AssetName) + "?token=" + testUploadSecret
		writeJSON(w, map[string]any{
			"upload_url":     uploadURL,
			"verify_url":     verifyURL,
			"expires_in_sec": 900,
		})
	case strings.HasPrefix(path, "/presigned/") && r.Method == http.MethodPut:
		if r.URL.Query().Get("token") != testUploadSecret {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if c.failUpload {
			http.Error(w, "storage unavailable", http.StatusInternalServerError)
			return
		}
		name, err := url.PathUnescape(strings.TrimPrefix(path, "/presigned/"))
		if err != nil {
			http.Error(w, "bad asset", http.StatusBadRequest)
			return
		}
		body, err := readAllBytes(r)
		if err != nil {
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.uploads[name] = body
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(path, "/verify/") && r.Method == http.MethodPost:
		if c.failVerify {
			http.Error(w, "verification failed", http.StatusInternalServerError)
			return
		}
		// 真实 CNB 在确认后把资产落账；mock 同样在此刻登记资产哈希。
		name, err := url.PathUnescape(strings.TrimPrefix(path, "/verify/"))
		if err != nil {
			http.Error(w, "bad asset", http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		data := c.uploads[name]
		c.mu.Unlock()
		c.refreshReleaseAsset(name, data)
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, `{"message":"unknown endpoint"}`, http.StatusNotFound)
	}
}

// refreshReleaseAsset 在 verify 成功后把资产登记到 CNB Release。
func (c *cnbTestServer) refreshReleaseAsset(name string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	release, ok := c.releases["v0.1.0"]
	if !ok {
		return
	}
	sum := sha256.Sum256(data)
	asset := cnbTestAsset{
		Name:     name,
		HashAlgo: "sha256",
		Hash:     hex.EncodeToString(sum[:]),
		Size:     int64(len(data)),
	}
	for i, existing := range release.Assets {
		if existing.Name == name {
			release.Assets[i] = asset
			return
		}
	}
	release.Assets = append(release.Assets, asset)
}

// seedRelease 预置一个已存在的 CNB Release，EXE 资产以 GitHub 内容为准，
// 校验和文件用 missingSums 判断是否要预置；同时把资产字节放入 uploads 供公开下载。
func (c *cnbTestServer) seedRelease(g *fakeGitHub, id, exeHash string, includeSums bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	release := &cnbTestRelease{
		ID: id, TagName: "v0.1.0",
		Name: "AUTO-MAS Runtime v0.1.0", TagCommitish: testCommit,
	}
	release.Assets = append(release.Assets, cnbTestAsset{
		Name: "auto-mas-runtime-v0.1.0.exe", HashAlgo: "sha256", Hash: exeHash, Size: int64(len(g.exeContent)),
	})
	c.uploads["auto-mas-runtime-v0.1.0.exe"] = g.exeContent
	if includeSums {
		sums := []byte(g.sumsContent())
		release.Assets = append(release.Assets, cnbTestAsset{
			Name: "SHA256SUMS.txt", HashAlgo: "sha256", Hash: sha256Hex(sums), Size: int64(len(sums)),
		})
		c.uploads["SHA256SUMS.txt"] = sums
	}
	c.releases["v0.1.0"] = release
}

func (c *cnbTestServer) routePublic(w http.ResponseWriter, r *http.Request) {
	c.log(r.Method, r.URL.Path)
	// /<org>/<repo>/-/releases/download/<tag>/<name>
	segments := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(segments) < 7 || segments[2] != "-" || segments[3] != "releases" || segments[4] != "download" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	name := segments[6]
	c.mu.Lock()
	data, ok := c.uploads[name]
	c.mu.Unlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/vnd.cnb.api+json")
	data, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "marshal failed", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(data)
}

func readAllBytes(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	var data []byte
	buffer := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buffer)
		data = append(data, buffer[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return data, nil
			}
			return nil, err
		}
	}
}

// fakeGitHub 模拟 GitHub Release/资产下载/Tag API 的最小子集。
type fakeGitHub struct {
	t          *testing.T
	server     *httptest.Server
	exeContent []byte
	sums       string
	digest     string
	releaseUp  bool // false → 404
	tagUp      bool
}

func newFakeGitHub(t *testing.T, exeContent []byte) *fakeGitHub {
	t.Helper()
	sum := sha256.Sum256(exeContent)
	g := &fakeGitHub{
		t:          t,
		exeContent: exeContent,
		digest:     hex.EncodeToString(sum[:]),
		releaseUp:  true,
		tagUp:      true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", g.route)
	g.server = httptest.NewServer(mux)
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGitHub) sumsContent() string {
	return fmt.Sprintf("%s  auto-mas-runtime-v0.1.0.exe\n", g.digest)
}

func (g *fakeGitHub) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/releases/tags/v0.1.0"):
		if !g.releaseUp {
			http.Error(w, `{}`, http.StatusNotFound)
			return
		}
		writeJSONRaw(w, map[string]any{
			"name":       "AUTO-MAS Runtime v0.1.0",
			"draft":      false,
			"prerelease": false,
			"body":       "Windows x64 build.",
			"assets": []map[string]any{
				{
					"name":   "auto-mas-runtime-v0.1.0.exe",
					"size":   len(g.exeContent),
					"digest": "sha256:" + g.digest,
					"url":    g.server.URL + "/assets/auto-mas-runtime-v0.1.0.exe",
				},
				{
					"name":   "SHA256SUMS.txt",
					"size":   len(g.sumsContent()),
					"digest": "sha256:" + sha256Hex([]byte(g.sumsContent())),
					"url":    g.server.URL + "/assets/SHA256SUMS.txt",
				},
			},
		})
	case strings.HasPrefix(path, "/assets/"):
		name := strings.TrimPrefix(path, "/assets/")
		switch name {
		case "auto-mas-runtime-v0.1.0.exe":
			_, _ = w.Write(g.exeContent)
		case "SHA256SUMS.txt":
			_, _ = w.Write([]byte(g.sumsContent()))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	case strings.HasSuffix(path, "/git/ref/tags/v0.1.0"):
		if !g.tagUp {
			http.Error(w, `{}`, http.StatusNotFound)
			return
		}
		writeJSONRaw(w, map[string]any{
			"object": map[string]any{"type": "commit", "sha": testCommit},
		})
	default:
		http.Error(w, `{}`, http.StatusNotFound)
	}
}

func writeJSONRaw(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	data, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "marshal failed", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(data)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// prepareAssets 把两个资产写进临时目录并返回目录路径。
func prepareAssets(t *testing.T, g *fakeGitHub) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auto-mas-runtime-v0.1.0.exe"), g.exeContent, 0o644); err != nil {
		t.Fatalf("WriteFile(exe) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS.txt"), []byte(g.sumsContent()), 0o644); err != nil {
		t.Fatalf("WriteFile(sums) error = %v", err)
	}
	return dir
}

// runPublisher 启动发布脚本并返回合并输出。
func runPublisher(t *testing.T, githubURL, cnbURL, cnbPublicURL, assetsDir string, extra ...string) (string, error) {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return the test file")
	}
	scriptPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "scripts", "publish-cnb-release.ps1")
	args := []string{
		"-NoProfile", "-NonInteractive", "-File", scriptPath,
		"-GitHubToken", "github-token-test",
		"-GitHubApiUrl", githubURL,
		"-GitHubRepository", "AUTO-MAS-Project/AUTO-MAS-Runtime",
		"-CnbToken", testToken,
		"-CnbApiUrl", cnbURL,
		"-CnbRepository", "AUTO-MAS-Project/AUTO-MAS-Runtime",
		"-CnbDownloadBase", cnbPublicURL,
		"-Tag", "v0.1.0",
		"-ExpectedCommit", testCommit,
		"-AssetsDir", assetsDir,
		"-TagWaitSeconds", "2",
		"-TagPollIntervalSeconds", "1",
	}
	args = append(args, extra...)
	cmd := exec.Command("pwsh", args...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func TestPublishCnbScript_EndToEnd(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("publisher integration test requires pwsh, which this suite only provisions on Windows")
	}
	exeContent := []byte("fake runtime executable binary payload for CNB publisher tests")

	testCases := []struct {
		name        string
		setup       func(c *cnbTestServer, g *fakeGitHub)
		wantError   bool
		wantSigned  bool // 期望发生 PUT 上传
		wantCreated bool // 期望发生 Release 创建
		errFragment string
	}{
		{
			name: "first publish creates release and uploads both assets",
			setup: func(c *cnbTestServer, _ *fakeGitHub) {
				c.tags["v0.1.0"] = cnbTestTag{Target: testCommit, TargetType: "commit"}
			},
			wantSigned:  true,
			wantCreated: true,
		},
		{
			name: "idempotent rerun on identical release",
			setup: func(c *cnbTestServer, g *fakeGitHub) {
				c.tags["v0.1.0"] = cnbTestTag{Target: testCommit, TargetType: "commit"}
				c.seedRelease(g, "release-existing", sha256Hex(exeContent), true)
			},
			wantSigned: false,
		},
		{
			name: "backfills only the missing asset",
			setup: func(c *cnbTestServer, g *fakeGitHub) {
				c.tags["v0.1.0"] = cnbTestTag{Target: testCommit, TargetType: "commit"}
				c.seedRelease(g, "release-existing", sha256Hex(exeContent), false)
			},
			wantSigned: true,
		},
		{
			name: "refuses when CNB tag points elsewhere",
			setup: func(c *cnbTestServer, _ *fakeGitHub) {
				c.tags["v0.1.0"] = cnbTestTag{Target: "ffffffffffffffffffffffffffffffffffffffff", TargetType: "commit"}
			},
			wantError:   true,
			errFragment: "ffffffff",
		},
		{
			name: "refuses existing asset with different bytes",
			setup: func(c *cnbTestServer, _ *fakeGitHub) {
				c.tags["v0.1.0"] = cnbTestTag{Target: testCommit, TargetType: "commit"}
				c.releases["v0.1.0"] = &cnbTestRelease{
					ID: "release-existing", TagName: "v0.1.0",
					Name: "AUTO-MAS Runtime v0.1.0", TagCommitish: testCommit,
					Assets: []cnbTestAsset{
						{Name: "auto-mas-runtime-v0.1.0.exe", HashAlgo: "sha256", Hash: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", Size: 3},
					},
				}
			},
			wantError:   true,
			errFragment: "refusing to overwrite",
		},
		{
			name: "fails closed when upload confirmation fails",
			setup: func(c *cnbTestServer, _ *fakeGitHub) {
				c.tags["v0.1.0"] = cnbTestTag{Target: testCommit, TargetType: "commit"}
				c.failVerify = true
			},
			wantError:  true,
			wantSigned: true,
		},
		{
			name: "fails closed on CNB API auth error",
			setup: func(c *cnbTestServer, _ *fakeGitHub) {
				c.failAuth = true
			},
			wantError: true,
		},
		{
			name: "waits for the tag to appear",
			setup: func(c *cnbTestServer, _ *fakeGitHub) {
				// 1.2 秒后才挂载 Tag，真正覆盖"未同步→重试→就绪"轮询路径。
				c.mountTagAfter(1200*time.Millisecond, cnbTestTag{Target: testCommit, TargetType: "commit"})
			},
			wantSigned:  true,
			wantCreated: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			cnb := newCnbTestServer(t)
			github := newFakeGitHub(t, exeContent)
			if testCase.setup != nil {
				testCase.setup(cnb, github)
			}
			assetsDir := prepareAssets(t, github)
			output, err := runPublisher(t, github.server.URL, cnb.server.URL, cnb.public.URL, assetsDir)

			if testCase.wantError {
				if err == nil {
					t.Fatalf("publisher expected failure but exit 0; output:\n%s", output)
				}
				if testCase.errFragment != "" && !strings.Contains(output, testCase.errFragment) {
					t.Fatalf("publisher error output missing %q:\n%s", testCase.errFragment, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("publisher failed: %v\noutput:\n%s", err, output)
			}
			if got := cnb.sawPath("/presigned/"); got != testCase.wantSigned {
				t.Fatalf("PUT upload happened = %v, want %v", got, testCase.wantSigned)
			}
			if got := cnb.sawPath("/-/releases") && testCase.wantCreated; got != testCase.wantCreated {
				t.Fatalf("release creation happened = %v, want %v", got, testCase.wantCreated)
			}
			// 全文日志不得包含 token、预签名 URL 与 Authorization。
			for _, forbidden := range []string{testToken, testUploadSecret, "Authorization", "upload_url"} {
				if strings.Contains(output, forbidden) {
					t.Fatalf("publisher log leaks %q:\n%s", forbidden, output)
				}
			}
			// 公开回读必须发生。
			if !cnb.sawPath("/-/releases/download/") {
				t.Fatal("publisher did not verify assets through the public download endpoint")
			}
		})
	}
}
