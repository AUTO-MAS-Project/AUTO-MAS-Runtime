package config

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	invalidSegmentCharacters    = `<>:"/\|?*`
	maxDynamicSegmentUTF16Units = 128
	maxWindowsSegmentUTF16Units = 255
)

func validateSegment(value string) error {
	if value == "" {
		return ErrInvalidSegment
	}
	for _, character := range value {
		if character <= '\x1f' {
			return ErrInvalidSegment
		}
	}
	if value == "." || value == ".." || filepath.VolumeName(value) != "" {
		return ErrInvalidSegment
	}
	if strings.ContainsAny(value, invalidSegmentCharacters) {
		return ErrInvalidSegment
	}
	if strings.HasSuffix(value, ".") || strings.HasSuffix(value, " ") {
		return ErrInvalidSegment
	}
	if isReservedDeviceName(value) {
		return ErrInvalidSegment
	}
	if utf16CodeUnits(value) > maxDynamicSegmentUTF16Units {
		return ErrInvalidSegment
	}
	return nil
}

func isReservedDeviceName(value string) bool {
	device := strings.ToUpper(strings.SplitN(value, ".", 2)[0])
	switch device {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(device) != 4 || device[3] < '1' || device[3] > '9' {
		return false
	}
	return device[:3] == "COM" || device[:3] == "LPT"
}

func utf16CodeUnits(value string) int {
	return len(utf16.Encode([]rune(value)))
}

func appendPartSuffix(name string) (string, error) {
	derived := name + ".part"
	if utf16CodeUnits(derived) > maxWindowsSegmentUTF16Units {
		return "", ErrInvalidSegment
	}
	return derived, nil
}

// RepoUpdateDir 返回本次仓库更新操作的暂存目录。
func (l *Layout) RepoUpdateDir(operationID string) (string, error) {
	if err := validateSegment(operationID); err != nil {
		return "", fmt.Errorf("validate repository update operation id: %w", err)
	}
	return filepath.Join(l.appRoot, "repo.update-"+operationID), nil
}

// RepoPreviousDir 返回本次仓库替换前的保留目录。
func (l *Layout) RepoPreviousDir(operationID string) (string, error) {
	if err := validateSegment(operationID); err != nil {
		return "", fmt.Errorf("validate repository previous operation id: %w", err)
	}
	return filepath.Join(l.appRoot, "repo.previous-"+operationID), nil
}

// UVVersionDir 返回指定 uv 版本的工具目录。
func (l *Layout) UVVersionDir(version string) (string, error) {
	if err := validateSegment(version); err != nil {
		return "", fmt.Errorf("validate uv version: %w", err)
	}
	return filepath.Join(l.paths.uvToolsDir, version), nil
}

// UVExecutable 返回指定 uv 版本的可执行文件路径。
func (l *Layout) UVExecutable(version string) (string, error) {
	directory, err := l.UVVersionDir(version)
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, "uv.exe"), nil
}

// DownloadFile 返回下载缓存中指定文件的路径。
func (l *Layout) DownloadFile(name string) (string, error) {
	if err := validateSegment(name); err != nil {
		return "", fmt.Errorf("validate download file name: %w", err)
	}
	return filepath.Join(l.paths.downloadCacheDir, name), nil
}

// DownloadPartFile 返回下载缓存中指定文件的分段下载路径。
func (l *Layout) DownloadPartFile(name string) (string, error) {
	if err := validateSegment(name); err != nil {
		return "", fmt.Errorf("validate download part file name: %w", err)
	}
	partName, err := appendPartSuffix(name)
	if err != nil {
		return "", fmt.Errorf("derive download part file name: %w", err)
	}
	return filepath.Join(l.paths.downloadCacheDir, partName), nil
}

// UVStagingDir 返回指定 uv 版本与操作的构建暂存目录。
func (l *Layout) UVStagingDir(version, operationID string) (string, error) {
	if err := validateSegment(version); err != nil {
		return "", fmt.Errorf("validate uv staging version: %w", err)
	}
	if err := validateSegment(operationID); err != nil {
		return "", fmt.Errorf("validate uv staging operation id: %w", err)
	}
	return filepath.Join(l.paths.buildCacheDir, "uv", version, operationID), nil
}

// RuntimeLogFile 返回指定命令在本地日期的运行日志文件路径。
func (l *Layout) RuntimeLogFile(command string, localDate time.Time) (string, error) {
	if err := validateSegment(command); err != nil {
		return "", fmt.Errorf("validate log command: %w", err)
	}
	if localDate.IsZero() {
		return "", ErrInvalidLogDate
	}
	name := command + "-" + localDate.Format("20060102") + ".log"
	return filepath.Join(l.paths.runtimeLogDir, name), nil
}
