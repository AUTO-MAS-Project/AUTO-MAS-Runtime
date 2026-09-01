package uv

import (
	"strings"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
)

const (
	// officialIndexPrefix 是 uv.lock 中 registry 条目的官方索引前缀。
	officialIndexPrefix = "https://pypi.org/simple"
	// officialArtifactPrefix 是 uv.lock 中 sdist/wheel URL 的官方 artifact 前缀。
	officialArtifactPrefix = "https://files.pythonhosted.org/packages/"
)

// lockRewriteResult 保存改写后的锁文本与两处前缀各自的替换次数。
type lockRewriteResult struct {
	Lock      string
	Indexes   int
	Artifacts int
}

// Total 返回两处前缀的替换总次数。
func (r lockRewriteResult) Total() int {
	return r.Indexes + r.Artifacts
}

// rewriteLockfile 把锁文本中的两个官方前缀替换为镜像前缀。
//
// 这是纯字符串前缀替换：不解析 TOML、不使用正则、不触碰 hash 与 size 字段。
// 锁内每个 artifact 的 sha256 因此逐字保留，由 uv 在 --frozen 安装时逐个校验——
// 镜像返回了不同的字节就会被拒装，Runtime 不需要再做额外校验。
//
// 用单遍 strings.Replacer 而不是两次 ReplaceAll：单遍扫描保证替换产生的文本
// 不会再被第二个模式命中，改写结果与两个模式的先后无关。
func rewriteLockfile(lock string, rewrite mirror.PackageIndexRewrite) lockRewriteResult {
	simpleBase := rewrite.SimpleBase()
	packagesBase := rewrite.PackagesBase()
	if simpleBase == "" || packagesBase == "" {
		return lockRewriteResult{Lock: lock}
	}
	replacer := strings.NewReplacer(
		officialArtifactPrefix, packagesBase,
		officialIndexPrefix, simpleBase,
	)
	return lockRewriteResult{
		Lock:      replacer.Replace(lock),
		Indexes:   strings.Count(lock, officialIndexPrefix),
		Artifacts: strings.Count(lock, officialArtifactPrefix),
	}
}
