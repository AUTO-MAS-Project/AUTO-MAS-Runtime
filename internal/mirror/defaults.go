package mirror

import "fmt"

// DefaultCatalog 通过生产校验路径构造冻结的内置 Source。
func DefaultCatalog() (*Catalog, error) {
	specs := []struct {
		kind         Kind
		key          string
		baseURL      string
		official     bool
		simpleBase   string
		packagesBase string
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
			kind:         KindPackageIndex,
			key:          "aliyun",
			baseURL:      "https://mirrors.aliyun.com/pypi/simple/",
			simpleBase:   "https://mirrors.aliyun.com/pypi/simple",
			packagesBase: "https://mirrors.aliyun.com/pypi/packages/",
		},
		{
			kind:         KindPackageIndex,
			key:          "tsinghua",
			baseURL:      "https://pypi.tuna.tsinghua.edu.cn/simple/",
			simpleBase:   "https://pypi.tuna.tsinghua.edu.cn/simple",
			packagesBase: "https://pypi.tuna.tsinghua.edu.cn/packages/",
		},
		{
			kind:         KindPackageIndex,
			key:          "ustc",
			baseURL:      "https://pypi.mirrors.ustc.edu.cn/simple/",
			simpleBase:   "https://pypi.mirrors.ustc.edu.cn/simple",
			packagesBase: "https://pypi.mirrors.ustc.edu.cn/packages/",
		},
		{
			// 官方源的 artifact 在 files.pythonhosted.org，与索引不同 host。
			// 它同时是被改写的源侧前缀，因此这里的两个值就是锁文件里的原始前缀。
			kind:         KindPackageIndex,
			key:          "pypi",
			baseURL:      "https://pypi.org/simple/",
			official:     true,
			simpleBase:   "https://pypi.org/simple",
			packagesBase: "https://files.pythonhosted.org/packages/",
		},
	}
	sources := make([]Source, 0, len(specs))
	for _, spec := range specs {
		var (
			source Source
			err    error
		)
		if spec.kind == KindPackageIndex {
			source, err = NewPackageIndexSource(PackageIndexSpec{
				Key:          spec.key,
				BaseURL:      spec.baseURL,
				SimpleBase:   spec.simpleBase,
				PackagesBase: spec.packagesBase,
				Official:     spec.official,
			})
		} else {
			source, err = NewSource(
				spec.kind,
				spec.key,
				spec.baseURL,
				spec.official,
			)
		}
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
