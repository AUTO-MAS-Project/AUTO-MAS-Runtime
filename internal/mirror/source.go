package mirror

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

var (
	errInvalidSource  = errors.New("mirror source is invalid")
	errInvalidCatalog = errors.New("mirror catalog is invalid")
)

type sourceURLParseError struct {
	cause error
}

func (e *sourceURLParseError) Error() string {
	return "mirror source URL syntax is invalid"
}

func (e *sourceURLParseError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return []error{errInvalidSource, e.cause}
}

// Source 是经过校验的不可变 HTTPS 网络源。
type Source struct {
	kind     Kind
	key      string
	baseURL  string
	official bool
}

// NewSource 校验并规范化单个网络源。
func NewSource(
	kind Kind,
	key string,
	baseURL string,
	official bool,
) (Source, error) {
	if !kind.Valid() || !validSourceKey(key) {
		return Source{}, fmt.Errorf("%w: kind or key", errInvalidSource)
	}
	normalized, err := normalizeSourceURL(baseURL)
	if err != nil {
		return Source{}, err
	}
	return Source{
		kind:     kind,
		key:      key,
		baseURL:  normalized,
		official: official,
	}, nil
}

// Kind 返回 Source 所属类别。
func (s Source) Kind() Kind {
	return s.kind
}

// Key 返回同一 Kind 内唯一的稳定 key。
func (s Source) Key() string {
	return s.key
}

// BaseURL 返回规范化 HTTPS URL 字符串。
func (s Source) BaseURL() string {
	return s.baseURL
}

// Official 报告 Source 是否为该 Kind 的官方源。
func (s Source) Official() bool {
	return s.official
}

// Catalog 保存按 Kind 分组的不可变有序 Source。
type Catalog struct {
	sources map[Kind][]Source
}

// NewCatalog 校验并防御性复制完整的四类 Source。
func NewCatalog(sources []Source) (*Catalog, error) {
	grouped := make(map[Kind][]Source, len(AllKinds()))
	keys := make(map[Kind]map[string]struct{}, len(AllKinds()))
	officialCounts := make(map[Kind]int, len(AllKinds()))
	for _, source := range sources {
		if err := validateSource(source); err != nil {
			return nil, fmt.Errorf("%w: source invariant", errInvalidCatalog)
		}
		if keys[source.kind] == nil {
			keys[source.kind] = make(map[string]struct{})
		}
		if _, exists := keys[source.kind][source.key]; exists {
			return nil, fmt.Errorf("%w: duplicate source key", errInvalidCatalog)
		}
		keys[source.kind][source.key] = struct{}{}
		grouped[source.kind] = append(grouped[source.kind], source)
		if source.official {
			officialCounts[source.kind]++
		}
	}
	for _, kind := range AllKinds() {
		if officialCounts[kind] != 1 {
			return nil, fmt.Errorf("%w: each kind needs one official source", errInvalidCatalog)
		}
		grouped[kind] = append([]Source(nil), grouped[kind]...)
	}
	return &Catalog{sources: grouped}, nil
}

// Sources 按配置顺序返回 kind 的 Source 防御性副本。
func (c *Catalog) Sources(kind Kind) []Source {
	if c == nil || !kind.Valid() {
		return nil
	}
	return append([]Source(nil), c.sources[kind]...)
}

// Source 按 Kind 与 key 查找一个不可变 Source。
func (c *Catalog) Source(kind Kind, key string) (Source, bool) {
	if c == nil || !kind.Valid() || !validSourceKey(key) {
		return Source{}, false
	}
	for _, source := range c.sources[kind] {
		if source.key == key {
			return source, true
		}
	}
	return Source{}, false
}

func normalizeSourceURL(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("%w: URL", errInvalidSource)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", &sourceURLParseError{cause: err}
	}
	if !parsed.IsAbs() ||
		parsed.Opaque != "" ||
		!strings.EqualFold(parsed.Scheme, "https") ||
		parsed.Hostname() == "" ||
		parsed.User != nil ||
		parsed.RawQuery != "" ||
		parsed.ForceQuery ||
		parsed.Fragment != "" ||
		parsed.RawFragment != "" ||
		strings.Contains(value, "#") ||
		strings.ContainsRune(parsed.Path, '\x00') ||
		strings.ContainsRune(parsed.RawPath, '\x00') {
		return "", fmt.Errorf("%w: URL policy", errInvalidSource)
	}
	parsed.Scheme = "https"
	parsed.Host = strings.ToLower(parsed.Host)
	return parsed.String(), nil
}

func validateSource(source Source) error {
	normalized, err := NewSource(
		source.kind,
		source.key,
		source.baseURL,
		source.official,
	)
	if err != nil || normalized != source {
		return errInvalidSource
	}
	return nil
}
