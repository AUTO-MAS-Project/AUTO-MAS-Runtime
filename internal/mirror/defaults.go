package mirror

import "fmt"

// DefaultCatalog 通过生产校验路径构造冻结的内置 Source。
func DefaultCatalog() (*Catalog, error) {
	specs := []struct {
		kind     Kind
		key      string
		baseURL  string
		official bool
	}{
		{
			kind:    KindGit,
			key:     "cnb",
			baseURL: "https://cnb.cool/AUTO-MAS-Project/AUTO-MAS.git",
		},
		{
			kind:     KindGit,
			key:      "github",
			baseURL:  "https://github.com/AUTO-MAS-Project/AUTO-MAS.git",
			official: true,
		},
		{
			kind:    KindUV,
			key:     "agentsmirror",
			baseURL: "https://uv.agentsmirror.com/github/astral-sh/uv/releases/download",
		},
		{
			kind:    KindUV,
			key:     "gh-proxy",
			baseURL: "https://gh-proxy.com/https://github.com/astral-sh/uv/releases/download",
		},
		{
			kind:    KindUV,
			key:     "cdn-gh-proxy",
			baseURL: "https://cdn.gh-proxy.com/https://github.com/astral-sh/uv/releases/download",
		},
		{
			kind:    KindUV,
			key:     "edgeone-gh-proxy",
			baseURL: "https://edgeone.gh-proxy.com/https://github.com/astral-sh/uv/releases/download",
		},
		{
			kind:     KindUV,
			key:      "github",
			baseURL:  "https://github.com/astral-sh/uv/releases/download",
			official: true,
		},
		{
			kind:    KindPython,
			key:     "gh-proxy",
			baseURL: "https://gh-proxy.com/https://github.com/astral-sh/python-build-standalone/releases/download",
		},
		{
			kind:     KindPython,
			key:      "github",
			baseURL:  "https://github.com/astral-sh/python-build-standalone/releases/download",
			official: true,
		},
		{
			kind:    KindPackageIndex,
			key:     "aliyun",
			baseURL: "https://mirrors.aliyun.com/pypi/simple/",
		},
		{
			kind:    KindPackageIndex,
			key:     "tsinghua",
			baseURL: "https://pypi.tuna.tsinghua.edu.cn/simple/",
		},
		{
			kind:    KindPackageIndex,
			key:     "ustc",
			baseURL: "https://pypi.mirrors.ustc.edu.cn/simple/",
		},
		{
			kind:     KindPackageIndex,
			key:      "pypi",
			baseURL:  "https://pypi.org/simple/",
			official: true,
		},
	}
	sources := make([]Source, 0, len(specs))
	for _, spec := range specs {
		source, err := NewSource(
			spec.kind,
			spec.key,
			spec.baseURL,
			spec.official,
		)
		if err != nil {
			return nil, fmt.Errorf("build default source: %w", err)
		}
		sources = append(sources, source)
	}
	catalog, err := NewCatalog(sources)
	if err != nil {
		return nil, fmt.Errorf("build default catalog: %w", err)
	}
	return catalog, nil
}
