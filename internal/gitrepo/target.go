package gitrepo

import (
	"errors"
	"strings"
)

const (
	minVersionBytes = 2
	maxVersionBytes = 128
	releasePrefix   = "release/"
)

// ErrInvalidVersion 表示产品版本不能安全映射为发布分支。
var ErrInvalidVersion = errors.New("version is invalid")

// Target 保存经校验的产品版本及其唯一发布分支。
type Target struct {
	version string
	branch  string
}

// ParseTarget 校验产品版本并按固定模板创建目标。
func ParseTarget(version string) (Target, error) {
	if !validVersion(version) {
		return Target{}, ErrInvalidVersion
	}
	return Target{
		version: version,
		branch:  releasePrefix + version,
	}, nil
}

// Version 返回未经规范化的完整产品版本。
func (t Target) Version() string {
	return t.version
}

// Branch 返回产品版本唯一派生的发布分支。
func (t Target) Branch() string {
	return t.branch
}

func (t Target) validate() error {
	parsed, err := ParseTarget(t.version)
	if err != nil || parsed != t {
		return ErrInvalidVersion
	}
	return nil
}

func validVersion(version string) bool {
	if len(version) < minVersionBytes || len(version) > maxVersionBytes || version[0] != 'v' {
		return false
	}
	if strings.Contains(version, "..") ||
		strings.Contains(version, "@{") ||
		strings.HasSuffix(version, ".") ||
		strings.HasSuffix(version, ".lock") {
		return false
	}
	for i := 1; i < len(version); i++ {
		character := version[i]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '.' ||
			character == '-' ||
			character == '_' {
			continue
		}
		return false
	}
	return true
}
