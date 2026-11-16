//go:build !darwin

package config

// SystemPath 在无固定根级系统别名的平台保留原路径。
func SystemPath(path string) string { return path }
