package doctor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// probeUVTimeout 是生产 uv 版本探测的固定超时。
// 15 秒足以覆盖正常 --version 执行与慢盘启动，同时保证挂死的 uv.exe
// 不会让 doctor 无法结束（M3 尚无 stdin cancel 通道）。
const probeUVTimeout = 15 * time.Second

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
	return probeUVVersionWithTimeout(ctx, exePath, probeUVTimeout)
}

// probeUVVersionWithTimeout 以独立超时上下文执行 uv --version。
// 超时到期后 exec.CommandContext 终止子进程并返回包装
// context.DeadlineExceeded 的错误，errorKind 将其分类为 timeout。
func probeUVVersionWithTimeout(
	ctx context.Context,
	exePath string,
	timeout time.Duration,
) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(probeCtx, exePath, "--version")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		// Windows 上 TerminateProcess 的退出码不携带 context 错误，
		// 显式以探测上下文错误收口，保证错误链可被 errorKind 分类为 timeout。
		if probeCtx.Err() != nil {
			return "", fmt.Errorf("run uv --version: %w", probeCtx.Err())
		}
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
