package config

import (
	"path/filepath"
	"strings"
)

// SystemPath 展开 macOS 固定的根级系统别名，其他用户链接保持原样供安全检查拒绝。
func SystemPath(path string) string {
	path = filepath.Clean(path)
	for _, alias := range []string{"/var", "/tmp", "/etc"} {
		if path == alias || strings.HasPrefix(path, alias+"/") {
			return "/private" + path
		}
	}
	return path
}
