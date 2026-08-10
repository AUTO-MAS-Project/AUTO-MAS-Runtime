package mirror

import (
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestKind_AllKindsStringAndValid(t *testing.T) {
	want := []Kind{
		KindGit,
		KindUV,
		KindPython,
		KindPackageIndex,
	}
	got := AllKinds()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AllKinds() = %#v, want %#v", got, want)
	}
	got[0] = Kind("changed")
	if fresh := AllKinds(); !reflect.DeepEqual(fresh, want) {
		t.Fatalf("AllKinds() exposed shared storage: %#v", fresh)
	}

	for _, kind := range want {
		if !kind.Valid() || kind.String() != string(kind) {
			t.Errorf("Kind(%q) = valid %t/string %q", kind, kind.Valid(), kind.String())
		}
	}
	for _, kind := range []Kind{"", "Git", "package_index", "unknown"} {
		if kind.Valid() {
			t.Errorf("Kind(%q).Valid() = true, want false", kind)
		}
	}
}

func TestNewSource_ValidatesAndNormalizes(t *testing.T) {
	source, err := NewSource(
		KindUV,
		"gh-proxy",
		"HTTPS://EXAMPLE.COM/releases/download",
		false,
	)
	if err != nil {
		t.Fatalf("NewSource() error = %v", err)
	}
	if source.Kind() != KindUV ||
		source.Key() != "gh-proxy" ||
		source.BaseURL() != "https://example.com/releases/download" ||
		source.Official() {
		t.Fatalf("Source = %#v", source)
	}

	tests := []struct {
		name    string
		kind    Kind
		key     string
		baseURL string
	}{
		{name: "unknown kind", kind: Kind("unknown"), key: "source", baseURL: "https://example.com"},
		{name: "empty key", kind: KindGit, key: "", baseURL: "https://example.com"},
		{name: "uppercase key", kind: KindGit, key: "GitHub", baseURL: "https://example.com"},
		{name: "underscore key", kind: KindGit, key: "git_hub", baseURL: "https://example.com"},
		{name: "leading hyphen", kind: KindGit, key: "-github", baseURL: "https://example.com"},
		{name: "double hyphen", kind: KindGit, key: "git--hub", baseURL: "https://example.com"},
		{name: "http", kind: KindGit, key: "github", baseURL: "http://example.com/repo.git"},
		{name: "relative", kind: KindGit, key: "github", baseURL: "example/repo.git"},
		{name: "opaque", kind: KindGit, key: "github", baseURL: "https:repository"},
		{name: "empty host", kind: KindGit, key: "github", baseURL: "https:///repo.git"},
		{name: "userinfo", kind: KindGit, key: "github", baseURL: "https://user:secret@example.com/repo.git"},
		{name: "query", kind: KindGit, key: "github", baseURL: "https://example.com/repo.git?token=secret"},
		{name: "empty query", kind: KindGit, key: "github", baseURL: "https://example.com/repo.git?"},
		{name: "fragment", kind: KindGit, key: "github", baseURL: "https://example.com/repo.git#main"},
		{name: "nul", kind: KindGit, key: "github", baseURL: "https://example.com/\x00repo"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewSource(test.kind, test.key, test.baseURL, false)
			if err == nil {
				t.Fatal("NewSource() error = nil, want validation error")
			}
			for _, secret := range []string{"secret", "token="} {
				if strings.Contains(strings.ToLower(err.Error()), secret) {
					t.Fatalf("NewSource() error leaked input: %v", err)
				}
			}
		})
	}
}

func TestNewSource_ParseErrorsAreSafelyWrapped(t *testing.T) {
	const malformedURL = "https://user:password@example.invalid/%zz?token=secret#fragment"
	_, err := NewSource(
		KindGit,
		"github",
		malformedURL,
		false,
	)
	if err == nil {
		t.Fatal("NewSource() error = nil, want URL parse error")
	}
	if err.Error() != "mirror source URL syntax is invalid" {
		t.Fatalf(
			"NewSource() error = %q, want stable URL syntax text",
			err.Error(),
		)
	}
	for _, forbidden := range []string{
		"user:password",
		"example.invalid",
		"token=secret",
		"fragment",
		"%zz",
		"https://",
	} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf(
				"NewSource() error = %q, contains %q",
				err.Error(),
				forbidden,
			)
		}
	}
	var parseErr *url.Error
	if !errors.Is(err, errInvalidSource) ||
		!errors.As(err, &parseErr) ||
		parseErr == nil ||
		!errors.Is(err, parseErr) ||
		!errors.Is(err, parseErr.Err) {
		t.Fatalf(
			"NewSource() error = %v, want invalid-source and parser cause chain",
			err,
		)
	}
}

func TestDefaultCatalog_ExactSourcesAndOrder(t *testing.T) {
	catalog, err := DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	tests := []struct {
		kind Kind
		want []struct {
			key      string
			baseURL  string
			official bool
		}
	}{
		{
			kind: KindGit,
			want: []struct {
				key      string
				baseURL  string
				official bool
			}{
				{
					key:     "cnb",
					baseURL: "https://cnb.cool/AUTO-MAS-Project/AUTO-MAS.git",
				},
				{
					key:      "github",
					baseURL:  "https://github.com/AUTO-MAS-Project/AUTO-MAS.git",
					official: true,
				},
			},
		},
		{
			kind: KindUV,
			want: []struct {
				key      string
				baseURL  string
				official bool
			}{
				{
					key:     "agentsmirror",
					baseURL: "https://uv.agentsmirror.com/github/astral-sh/uv/releases/download",
				},
				{
					key:     "gh-proxy",
					baseURL: "https://gh-proxy.com/https://github.com/astral-sh/uv/releases/download",
				},
				{
					key:     "cdn-gh-proxy",
					baseURL: "https://cdn.gh-proxy.com/https://github.com/astral-sh/uv/releases/download",
				},
				{
					key:     "edgeone-gh-proxy",
					baseURL: "https://edgeone.gh-proxy.com/https://github.com/astral-sh/uv/releases/download",
				},
				{
					key:      "github",
					baseURL:  "https://github.com/astral-sh/uv/releases/download",
					official: true,
				},
			},
		},
		{
			kind: KindPython,
			want: []struct {
				key      string
				baseURL  string
				official bool
			}{
				{
					key:     "gh-proxy",
					baseURL: "https://gh-proxy.com/https://github.com/astral-sh/python-build-standalone/releases/download",
				},
				{
					key:      "github",
					baseURL:  "https://github.com/astral-sh/python-build-standalone/releases/download",
					official: true,
				},
			},
		},
		{
			kind: KindPackageIndex,
			want: []struct {
				key      string
				baseURL  string
				official bool
			}{
				{key: "aliyun", baseURL: "https://mirrors.aliyun.com/pypi/simple/"},
				{key: "tsinghua", baseURL: "https://pypi.tuna.tsinghua.edu.cn/simple/"},
				{key: "ustc", baseURL: "https://pypi.mirrors.ustc.edu.cn/simple/"},
				{
					key:      "pypi",
					baseURL:  "https://pypi.org/simple/",
					official: true,
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.kind.String(), func(t *testing.T) {
			got := catalog.Sources(test.kind)
			if len(got) != len(test.want) {
				t.Fatalf("Sources(%q) length = %d, want %d", test.kind, len(got), len(test.want))
			}
			for i, want := range test.want {
				if got[i].Kind() != test.kind ||
					got[i].Key() != want.key ||
					got[i].BaseURL() != want.baseURL ||
					got[i].Official() != want.official {
					t.Errorf("Sources(%q)[%d] = %#v, want %#v", test.kind, i, got[i], want)
				}
				if !strings.HasPrefix(got[i].BaseURL(), "https://") {
					t.Errorf("Sources(%q)[%d] URL = %q, want HTTPS", test.kind, i, got[i].BaseURL())
				}
			}
		})
	}
}

func TestCatalog_RejectsInvalidComposition(t *testing.T) {
	base := allDefaultSources(t)
	tests := []struct {
		name    string
		sources func() []Source
	}{
		{
			name: "duplicate key in kind",
			sources: func() []Source {
				return append(append([]Source(nil), base...), base[0])
			},
		},
		{
			name: "missing official",
			sources: func() []Source {
				result := make([]Source, 0, len(base)-1)
				for _, source := range base {
					if source.Kind() == KindGit && source.Official() {
						continue
					}
					result = append(result, source)
				}
				return result
			},
		},
		{
			name: "multiple official",
			sources: func() []Source {
				extra, err := NewSource(
					KindGit,
					"backup-official",
					"https://example.com/AUTO-MAS.git",
					true,
				)
				if err != nil {
					t.Fatalf("NewSource() error = %v", err)
				}
				return append(append([]Source(nil), base...), extra)
			},
		},
		{
			name: "forged source",
			sources: func() []Source {
				result := append([]Source(nil), base...)
				result[0] = Source{
					kind:     KindGit,
					key:      "cnb",
					baseURL:  "http://example.com/repo.git",
					official: false,
				}
				return result
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewCatalog(test.sources()); err == nil {
				t.Fatal("NewCatalog() error = nil, want validation error")
			}
		})
	}
}

func TestCatalog_DefensiveCopiesAndLookup(t *testing.T) {
	input := allDefaultSources(t)
	catalog, err := NewCatalog(input)
	if err != nil {
		t.Fatalf("NewCatalog() error = %v", err)
	}
	input[0] = Source{}

	gitSources := catalog.Sources(KindGit)
	if len(gitSources) != 2 || gitSources[0].Key() != "cnb" {
		t.Fatalf("Sources(git) = %#v", gitSources)
	}
	gitSources[0] = Source{}
	fresh := catalog.Sources(KindGit)
	if fresh[0].Key() != "cnb" {
		t.Fatalf("Sources() exposed shared storage: %#v", fresh)
	}

	source, ok := catalog.Source(KindGit, "github")
	if !ok || !source.Official() {
		t.Fatalf("Source(git, github) = %#v, %t", source, ok)
	}
	if _, ok := catalog.Source(KindGit, "missing"); ok {
		t.Fatal("Source(git, missing) ok = true, want false")
	}
	if got := catalog.Sources(Kind("unknown")); got != nil {
		t.Fatalf("Sources(unknown) = %#v, want nil", got)
	}
}

func TestSource_PublicSurfaceHasNoTLSBypass(t *testing.T) {
	sourceType := reflect.TypeOf(Source{})
	for i := 0; i < sourceType.NumField(); i++ {
		field := sourceType.Field(i)
		if field.IsExported() {
			t.Fatalf("Source field %q is exported", field.Name)
		}
		lower := strings.ToLower(field.Name)
		for _, forbidden := range []string{"insecure", "skipverify", "allowhttp", "ca"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("Source field %q exposes TLS bypass state", field.Name)
			}
		}
	}
}

func allDefaultSources(t *testing.T) []Source {
	t.Helper()
	catalog, err := DefaultCatalog()
	if err != nil {
		t.Fatalf("DefaultCatalog() error = %v", err)
	}
	var sources []Source
	for _, kind := range AllKinds() {
		sources = append(sources, catalog.Sources(kind)...)
	}
	return sources
}
