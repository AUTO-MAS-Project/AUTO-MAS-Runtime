package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows"
)

// ProductionProbes 返回生产环境探测实现。
func ProductionProbes() Probes {
	return Probes{
		UVVersion: probeUVVersion,
		DiskFree:  probeDiskFree,
	}
}

// probeUVVersion 执行受管 uv.exe --version 并返回规范化输出。
// T5.2 UVRunner 落地后，uv 子进程统一改由 internal/uv 的唯一执行器调用。
func probeUVVersion(ctx context.Context, exePath string) (string, error) {
	command := exec.CommandContext(ctx, exePath, "--version")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("run uv --version: %w", err)
	}
	version := strings.TrimSpace(stdout.String())
	if version == "" {
		return "", errors.New("uv --version produced no output")
	}
	return version, nil
}

// probeDiskFree 查询路径所在卷的可用字节数。
func probeDiskFree(ctx context.Context, path string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("encode disk path: %w", err)
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(
		pathPointer,
		&freeBytesAvailable,
		&totalBytes,
		&totalFreeBytes,
	); err != nil {
		return 0, fmt.Errorf("query disk free space: %w", err)
	}
	return freeBytesAvailable, nil
}
