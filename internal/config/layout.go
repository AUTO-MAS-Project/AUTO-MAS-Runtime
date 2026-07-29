package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Layout 保存一个 app root 的不可变受管目录布局。
type Layout struct {
	appRoot     string
	identityKey string
	paths       layoutPaths
}

// NewLayout 以显式绝对 base 解析 app root，不访问文件系统。
func NewLayout(appRoot, base string) (*Layout, error) {
	if appRoot == "" || base == "" {
		return nil, ErrEmptyPath
	}
	if strings.ContainsRune(appRoot, '\x00') || strings.ContainsRune(base, '\x00') {
		return nil, ErrPathContainsNUL
	}
	if !filepath.IsAbs(base) {
		return nil, ErrBaseNotAbsolute
	}
	if filepath.VolumeName(appRoot) != "" && !filepath.IsAbs(appRoot) {
		return nil, ErrInvalidPath
	}

	resolved := appRoot
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Clean(base), resolved)
	}
	resolved = filepath.Clean(resolved)
	if !filepath.IsAbs(resolved) {
		return nil, fmt.Errorf("resolve app root: %w", ErrInvalidPath)
	}
	if isVolumeRoot(resolved) {
		return nil, ErrAppRootIsRoot
	}

	return &Layout{
		appRoot:     resolved,
		identityKey: identityKey(resolved),
		paths:       newLayoutPaths(resolved),
	}, nil
}

// AppRoot 返回规范化的显示与文件 I/O 路径。
func (l *Layout) AppRoot() string {
	return l.appRoot
}

// IdentityKey 返回大小写不敏感的词法身份键。
func (l *Layout) IdentityKey() string {
	return l.identityKey
}

func identityKey(path string) string {
	normalized := filepath.Clean(path)
	normalized = strings.ReplaceAll(normalized, "/", `\`)
	return strings.ToLower(normalized)
}

func isVolumeRoot(path string) bool {
	volume := filepath.VolumeName(path)
	if volume == "" {
		return false
	}
	root := filepath.Clean(volume + string(filepath.Separator))
	return strings.EqualFold(filepath.Clean(path), root)
}
