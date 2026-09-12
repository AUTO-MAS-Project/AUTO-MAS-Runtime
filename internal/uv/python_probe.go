package uv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// pythonDownloadBases 是 uv 清单里可能出现的官方 base；探针路径就是 url 去掉其中之一后的余下部分。
var pythonDownloadBases = []string{
	"https://releases.astral.sh/github/python-build-standalone/releases/download/",
	"https://github.com/astral-sh/python-build-standalone/releases/download/",
}

var errPythonDownloadPath = errors.New("python download path is unavailable")

type pythonDownloadEntry struct {
	Version string          `json:"version"`
	URL     string          `json:"url"`
	OS      string          `json:"os"`
	Arch    json.RawMessage `json:"arch"`
}

// pythonDownloadPath 从 `uv python list --only-downloads --output-format json` 里取出
// Windows x86_64 目标版本的下载 URL，并返回去掉官方 base 后的相对路径（保留百分号编码）。
//
// 这是增补 2 C16 的 python 探针目标来源：uv 的 JSON 是机器输出而非自然语言，
// 且只在 uv 已就绪后调用；任何失败都只让 python Kind 退回目录顺序。
func pythonDownloadPath(ctx context.Context, runner Runner, options RunOptions, version PythonVersion) (string, error) {
	if runner == nil {
		return "", fmt.Errorf("%w: runner is nil", errPythonDownloadPath)
	}
	options = withOfflineUV(options)
	result, err := runner.Run(ctx, []string{
		"python",
		"list",
		version.String(),
		"--only-downloads",
		"--managed-python",
		"--output-format",
		"json",
	}, options)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("%w: uv exited with %d", errPythonDownloadPath, result.ExitCode)
	}
	var entries []pythonDownloadEntry
	if err := json.Unmarshal([]byte(result.Stdout), &entries); err != nil {
		return "", fmt.Errorf("%w: %w", errPythonDownloadPath, err)
	}
	expected := version.String()
	for _, entry := range entries {
		if entry.Version != expected || entry.OS != "windows" || !isX8664Arch(entry.Arch) {
			continue
		}
		for _, base := range pythonDownloadBases {
			if rest, ok := strings.CutPrefix(entry.URL, base); ok && rest != "" {
				return rest, nil
			}
		}
		return "", fmt.Errorf("%w: url %q is not under an official base", errPythonDownloadPath, entry.URL)
	}
	return "", fmt.Errorf("%w: no windows x86_64 download for %s", errPythonDownloadPath, expected)
}

// isX8664Arch 兼容 uv 清单里 arch 的两种形态：字符串 "x86_64" 或带 family 字段的对象。
func isX8664Arch(raw json.RawMessage) bool {
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain == "x86_64"
	}
	var object struct {
		Family string `json:"family"`
	}
	if json.Unmarshal(raw, &object) == nil {
		return object.Family == "x86_64"
	}
	return false
}
