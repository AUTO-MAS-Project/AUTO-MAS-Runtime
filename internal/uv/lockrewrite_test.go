package uv

import (
	"regexp"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

// lockFixture 是形态与真实 uv.lock 一致的最小锁文本：两个 registry 条目、
// 一个 sdist 与两个 wheel，外加一个既不是索引也不是 artifact 的干扰 URL。
const lockFixture = `version = 1
revision = 2
requires-python = ">=3.12, <3.13"

[[package]]
name = "certifi"
version = "2025.1.31"
source = { registry = "https://pypi.org/simple" }
sdist = { url = "https://files.pythonhosted.org/packages/1c/ab/c9f1e32b7b1bf505bf26f0ef697775960db7932abeb7b516de930ba2705f/certifi-2025.1.31.tar.gz", hash = "sha256:3d5da6925056f6f18f119200434a4780a94263f10d1c21d032a6f6b2baa20651", size = 167577 }
wheels = [
    { url = "https://files.pythonhosted.org/packages/38/fc/bce832fd4fd99766c04d1ee0eead6b0ec6486fb100ae5e74c1d91292b982/certifi-2025.1.31-py3-none-any.whl", hash = "sha256:ca78db4565a652026a4db2bcdf68f2fb589ea80d0be70e03929ed730746b84fe", size = 166393 },
]

[[package]]
name = "idna"
version = "3.10"
source = { registry = "https://pypi.org/simple" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/76/c6/c88e154df9c4e1a2a66ccf0005a88dfb2650c1dffb6f5ce603dfbd452ce3/idna-3.10-py3-none-any.whl", hash = "sha256:946d195a0d259cbba61165e88e65941f16e9b36ea6ddb97f00452bae8b1287d3", size = 70442 },
]

[[package]]
name = "auto-mas"
version = "5.5.0"
source = { editable = "." }

[package.metadata]
requires-dist = [{ name = "certifi" }, { name = "idna", specifier = ">=3.10" }]
# 干扰项：两个官方前缀都不匹配，必须原样保留。
# https://pypi.org/pypi/certifi/json https://files.pythonhosted.org/pypi/index
`

const (
	lockFixtureIndexes   = 2
	lockFixtureArtifacts = 3
)

func testRewrite(t *testing.T, simpleBase, packagesBase string) mirror.PackageIndexRewrite {
	t.Helper()
	source, err := mirror.NewPackageIndexSource(mirror.PackageIndexSpec{
		Key:          "tsinghua",
		BaseURL:      "https://pypi.tuna.tsinghua.edu.cn/simple/",
		SimpleBase:   simpleBase,
		PackagesBase: packagesBase,
	})
	if err != nil {
		t.Fatalf("mirror.NewPackageIndexSource() error = %v", err)
	}
	rewrite, ok := source.PackageIndexRewrite()
	if !ok {
		t.Fatal("PackageIndexRewrite() ok = false, want true")
	}
	return rewrite
}

func TestRewriteLockfile_ReplacesOnlyTheTwoExactPrefixes(t *testing.T) {
	rewrite := testRewrite(
		t,
		"https://pypi.tuna.tsinghua.edu.cn/simple",
		"https://pypi.tuna.tsinghua.edu.cn/packages/",
	)
	result := rewriteLockfile(lockFixture, rewrite)

	if result.Indexes != lockFixtureIndexes || result.Artifacts != lockFixtureArtifacts {
		t.Fatalf("counts = %d indexes/%d artifacts, want %d/%d",
			result.Indexes, result.Artifacts, lockFixtureIndexes, lockFixtureArtifacts)
	}
	if got, want := result.Total(), lockFixtureIndexes+lockFixtureArtifacts; got != want {
		t.Fatalf("Total() = %d, want %d", got, want)
	}
	if strings.Contains(result.Lock, "https://pypi.org/simple") ||
		strings.Contains(result.Lock, "https://files.pythonhosted.org/packages/") {
		t.Fatal("rewritten lock still contains an official prefix")
	}
	if got := strings.Count(result.Lock, `registry = "https://pypi.tuna.tsinghua.edu.cn/simple"`); got != lockFixtureIndexes {
		t.Errorf("rewritten registry entries = %d, want %d", got, lockFixtureIndexes)
	}
	if got := strings.Count(result.Lock, "https://pypi.tuna.tsinghua.edu.cn/packages/"); got != lockFixtureArtifacts {
		t.Errorf("rewritten artifact URLs = %d, want %d", got, lockFixtureArtifacts)
	}
	for _, untouched := range []string{
		"https://pypi.org/pypi/certifi/json",
		"https://files.pythonhosted.org/pypi/index",
	} {
		if !strings.Contains(result.Lock, untouched) {
			t.Errorf("rewritten lock lost an unrelated URL %q", untouched)
		}
	}
	if got, want := lineCount(result.Lock), lineCount(lockFixture); got != want {
		t.Errorf("line count = %d, want %d", got, want)
	}
}

func TestRewriteLockfile_KeepsHashesByteForByte(t *testing.T) {
	rewrite := testRewrite(
		t,
		"https://mirrors.aliyun.com/pypi/simple",
		"https://mirrors.aliyun.com/pypi/packages/",
	)
	result := rewriteLockfile(lockFixture, rewrite)

	hashPattern := regexp.MustCompile(`hash = "sha256:[0-9a-f]{64}"`)
	before := hashPattern.FindAllString(lockFixture, -1)
	after := hashPattern.FindAllString(result.Lock, -1)
	if len(before) != lockFixtureArtifacts {
		t.Fatalf("fixture hash lines = %d, want %d", len(before), lockFixtureArtifacts)
	}
	if len(after) != len(before) {
		t.Fatalf("hash lines after rewrite = %d, want %d", len(after), len(before))
	}
	for index := range before {
		if after[index] != before[index] {
			t.Errorf("hash[%d] = %q, want %q", index, after[index], before[index])
		}
	}
	if got, want := strings.Count(result.Lock, "size = "), strings.Count(lockFixture, "size = "); got != want {
		t.Errorf("size fields = %d, want %d", got, want)
	}
}

func TestRewriteLockfile_IsIdempotent(t *testing.T) {
	rewrite := testRewrite(
		t,
		"https://mirrors.aliyun.com/pypi/simple",
		"https://mirrors.aliyun.com/pypi/packages/",
	)
	first := rewriteLockfile(lockFixture, rewrite)
	second := rewriteLockfile(first.Lock, rewrite)

	if second.Lock != first.Lock {
		t.Error("second rewrite changed the lock text")
	}
	if second.Total() != 0 {
		t.Errorf("second rewrite replaced %d prefixes, want 0", second.Total())
	}
}

func TestRewriteLockfile_LeavesUnrelatedLockUnchanged(t *testing.T) {
	rewrite := testRewrite(
		t,
		"https://mirrors.aliyun.com/pypi/simple",
		"https://mirrors.aliyun.com/pypi/packages/",
	)
	const unrelated = "version = 1\nrequires-python = \">=3.12\"\n"
	result := rewriteLockfile(unrelated, rewrite)

	if result.Lock != unrelated {
		t.Errorf("lock = %q, want %q", result.Lock, unrelated)
	}
	if result.Total() != 0 {
		t.Errorf("Total() = %d, want 0", result.Total())
	}
}

func TestRewriteLockfile_OfficialPrefixesAreIdentity(t *testing.T) {
	// 官方源的两个前缀就是被替换的原串，因此改写在数据上是恒等变换。
	// 实际路径不会走到它（官方源用原锁），这条只是把不变量钉住。
	rewrite := testRewrite(
		t,
		"https://pypi.org/simple",
		"https://files.pythonhosted.org/packages/",
	)
	result := rewriteLockfile(lockFixture, rewrite)

	if result.Lock != lockFixture {
		t.Error("official rewrite changed the lock text")
	}
	if result.Indexes != lockFixtureIndexes || result.Artifacts != lockFixtureArtifacts {
		t.Errorf("counts = %d/%d, want %d/%d",
			result.Indexes, result.Artifacts, lockFixtureIndexes, lockFixtureArtifacts)
	}
}

func lineCount(text string) int {
	return strings.Count(text, "\n")
}
