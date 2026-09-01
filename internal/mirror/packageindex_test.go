package mirror

import (
	"errors"
	"strings"
	"testing"
)

func TestNewPackageIndexSource_ValidatesRewritePrefixes(t *testing.T) {
	cases := []struct {
		name string
		spec PackageIndexSpec
		want bool
	}{
		{
			name: "valid",
			spec: PackageIndexSpec{
				Key:          "aliyun",
				BaseURL:      "https://mirrors.aliyun.com/pypi/simple/",
				SimpleBase:   "https://mirrors.aliyun.com/pypi/simple",
				PackagesBase: "https://mirrors.aliyun.com/pypi/packages/",
			},
			want: true,
		},
		{
			name: "different hosts are allowed",
			spec: PackageIndexSpec{
				Key:          "pypi",
				BaseURL:      "https://pypi.org/simple/",
				SimpleBase:   "https://pypi.org/simple",
				PackagesBase: "https://files.pythonhosted.org/packages/",
				Official:     true,
			},
			want: true,
		},
		{
			name: "missing simple base",
			spec: PackageIndexSpec{
				Key:          "aliyun",
				BaseURL:      "https://mirrors.aliyun.com/pypi/simple/",
				PackagesBase: "https://mirrors.aliyun.com/pypi/packages/",
			},
		},
		{
			name: "missing packages base",
			spec: PackageIndexSpec{
				Key:        "aliyun",
				BaseURL:    "https://mirrors.aliyun.com/pypi/simple/",
				SimpleBase: "https://mirrors.aliyun.com/pypi/simple",
			},
		},
		{
			name: "simple base must not end with a slash",
			spec: PackageIndexSpec{
				Key:          "aliyun",
				BaseURL:      "https://mirrors.aliyun.com/pypi/simple/",
				SimpleBase:   "https://mirrors.aliyun.com/pypi/simple/",
				PackagesBase: "https://mirrors.aliyun.com/pypi/packages/",
			},
		},
		{
			name: "packages base must end with a slash",
			spec: PackageIndexSpec{
				Key:          "aliyun",
				BaseURL:      "https://mirrors.aliyun.com/pypi/simple/",
				SimpleBase:   "https://mirrors.aliyun.com/pypi/simple",
				PackagesBase: "https://mirrors.aliyun.com/pypi/packages",
			},
		},
		{
			name: "plain HTTP is rejected",
			spec: PackageIndexSpec{
				Key:          "aliyun",
				BaseURL:      "https://mirrors.aliyun.com/pypi/simple/",
				SimpleBase:   "http://mirrors.aliyun.com/pypi/simple",
				PackagesBase: "https://mirrors.aliyun.com/pypi/packages/",
			},
		},
		{
			name: "invalid key",
			spec: PackageIndexSpec{
				Key:          "Aliyun",
				BaseURL:      "https://mirrors.aliyun.com/pypi/simple/",
				SimpleBase:   "https://mirrors.aliyun.com/pypi/simple",
				PackagesBase: "https://mirrors.aliyun.com/pypi/packages/",
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source, err := NewPackageIndexSource(testCase.spec)
			if !testCase.want {
				if err == nil {
					t.Fatalf("NewPackageIndexSource(%#v) error = nil, want error", testCase.spec)
				}
				if !errors.Is(err, errInvalidSource) {
					t.Fatalf("NewPackageIndexSource() error = %v, want errInvalidSource", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewPackageIndexSource(%#v) error = %v", testCase.spec, err)
			}
			if source.Kind() != KindPackageIndex || source.Key() != testCase.spec.Key {
				t.Fatalf("source = %q/%q, want %q/%q", source.Kind(), source.Key(), KindPackageIndex, testCase.spec.Key)
			}
			rewrite, ok := source.PackageIndexRewrite()
			if !ok {
				t.Fatal("PackageIndexRewrite() ok = false, want true")
			}
			if rewrite.SimpleBase() != testCase.spec.SimpleBase {
				t.Errorf("SimpleBase() = %q, want %q", rewrite.SimpleBase(), testCase.spec.SimpleBase)
			}
			if rewrite.PackagesBase() != testCase.spec.PackagesBase {
				t.Errorf("PackagesBase() = %q, want %q", rewrite.PackagesBase(), testCase.spec.PackagesBase)
			}
			if err := validateSource(source); err != nil {
				t.Errorf("validateSource() error = %v, want nil", err)
			}
		})
	}
}

func TestSource_PackageIndexRewriteAbsentWithoutExplicitPrefixes(t *testing.T) {
	// NewSource 不接受改写前缀，因此它造出的包索引源不能参与锁改写：
	// 改写前缀必须显式声明，绝不由 baseURL 推导。
	source, err := NewSource(KindPackageIndex, "aliyun", "https://mirrors.aliyun.com/pypi/simple/", false)
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}
	if _, ok := source.PackageIndexRewrite(); ok {
		t.Fatal("PackageIndexRewrite() ok = true for a source without explicit prefixes, want false")
	}
	other, err := NewSource(KindUV, "github", "https://github.com/astral-sh/uv/releases/download", true)
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}
	if _, ok := other.PackageIndexRewrite(); ok {
		t.Fatal("PackageIndexRewrite() ok = true for a non package-index source, want false")
	}
}

func TestDefaultCatalog_PackageIndexRewrites(t *testing.T) {
	catalog, err := DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	want := []struct {
		key          string
		simpleBase   string
		packagesBase string
		official     bool
	}{
		{
			key:          "aliyun",
			simpleBase:   "https://mirrors.aliyun.com/pypi/simple",
			packagesBase: "https://mirrors.aliyun.com/pypi/packages/",
		},
		{
			key:          "tsinghua",
			simpleBase:   "https://pypi.tuna.tsinghua.edu.cn/simple",
			packagesBase: "https://pypi.tuna.tsinghua.edu.cn/packages/",
		},
		{
			key:          "ustc",
			simpleBase:   "https://pypi.mirrors.ustc.edu.cn/simple",
			packagesBase: "https://pypi.mirrors.ustc.edu.cn/packages/",
		},
		{
			key:          "pypi",
			simpleBase:   "https://pypi.org/simple",
			packagesBase: "https://files.pythonhosted.org/packages/",
			official:     true,
		},
	}
	sources := catalog.Sources(KindPackageIndex)
	if len(sources) != len(want) {
		t.Fatalf("package-index sources = %d, want %d", len(sources), len(want))
	}
	for index, expected := range want {
		source := sources[index]
		if source.Key() != expected.key || source.Official() != expected.official {
			t.Fatalf("source[%d] = %q/official %t, want %q/official %t",
				index, source.Key(), source.Official(), expected.key, expected.official)
		}
		rewrite, ok := source.PackageIndexRewrite()
		if !ok {
			t.Fatalf("source[%d] %q has no rewrite prefixes", index, source.Key())
		}
		if rewrite.SimpleBase() != expected.simpleBase {
			t.Errorf("source %q SimpleBase() = %q, want %q", source.Key(), rewrite.SimpleBase(), expected.simpleBase)
		}
		if rewrite.PackagesBase() != expected.packagesBase {
			t.Errorf("source %q PackagesBase() = %q, want %q", source.Key(), rewrite.PackagesBase(), expected.packagesBase)
		}
		if !strings.HasSuffix(rewrite.PackagesBase(), "/") || strings.HasSuffix(rewrite.SimpleBase(), "/") {
			t.Errorf("source %q rewrite prefixes violate the slash invariant: %q / %q",
				source.Key(), rewrite.SimpleBase(), rewrite.PackagesBase())
		}
	}
}
