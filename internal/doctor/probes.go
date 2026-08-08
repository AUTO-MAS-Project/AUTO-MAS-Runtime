package doctor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/uv"
)

// probeUVTimeout 是单次 uv 版本探测的固定超时。
// 15 秒足以覆盖正常 --version 执行与慢盘启动，同时保证挂死的 uv.exe
// 不会让 doctor 无法结束（M3 尚无 stdin cancel 通道）。
const probeUVTimeout = 15 * time.Second

// probeUVBudget 是 uv 检查项的总预算，覆盖对全部候选版本目录的枚举。
// 残留的历史版本目录会让 N 个候选各自消耗一次 probeUVTimeout；doctor 是
// 只读诊断，整项超出一次探测的预算后继续枚举没有价值（T3.8 F13c）。
const probeUVBudget = probeUVTimeout

// ProductionProbes 返回生产环境探测实现。
func ProductionProbes() Probes {
	return Probes{
		UVVersion: probeUVVersion,
		DiskFree:  probeDiskFree,
	}
}

// ProductionProbesForLayout 返回使用真实 managed runtime 目录的生产探针。
func ProductionProbesForLayout(layout *config.Layout) Probes {
	return Probes{
		UVVersionWithLayout: func(ctx context.Context, _ *config.Layout, exePath string) (string, error) {
			return probeUVVersionWithLayout(ctx, layout, exePath, probeUVTimeout)
		},
		DiskFree: probeDiskFree,
	}
}

// probeUVVersion 执行受管 uv.exe --version 并返回规范化输出。
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
	return probeUVVersionWithLayout(ctx, nil, exePath, timeout)
}

func probeUVVersionWithLayout(
	ctx context.Context,
	layout *config.Layout,
	exePath string,
	timeout time.Duration,
) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	base := filepath.Dir(exePath)
	projectDir := base
	pythonInstallDir := filepath.Join(base, "python")
	projectEnvDir := filepath.Join(base, "venv")
	cacheDir := filepath.Join(base, "cache")
	if layout != nil {
		// --version 不依赖后端仓库；doctor 需要能在首次安装的空 repo
		// 上继续报告 uv 事实，因此使用已存在的受管 app-root 作为工作目录。
		projectDir = layout.AppRoot()
		pythonInstallDir = layout.PythonDir()
		projectEnvDir = layout.VenvDir()
		cacheDir = layout.UVCacheDir()
	}
	runner, err := uv.NewRunner(uv.RunnerConfig{
		Executable:       exePath,
		ProjectDir:       projectDir,
		PythonInstallDir: pythonInstallDir,
		ProjectEnvDir:    projectEnvDir,
		CacheDir:         cacheDir,
	})
	if err != nil {
		return "", err
	}
	result, err := runner.Run(probeCtx, []string{"--version"}, uv.RunOptions{
		Stage: protocol.StageUVCheck,
	})
	if err != nil {
		if probeCtx.Err() != nil {
			return "", fmt.Errorf("run uv --version: %w", probeCtx.Err())
		}
		return "", fmt.Errorf("run uv --version: %w", err)
	}
	version := strings.TrimSpace(result.Stdout)
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
