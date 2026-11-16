package doctor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
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
		UVVersion:  probeUVVersion,
		DiskFree:   probeDiskFree,
		FileSystem: filesystem.FileSystemName,
	}
}

// ProductionProbesForLayout 返回使用真实 managed runtime 目录的生产探针。
func ProductionProbesForLayout(layout *config.Layout) Probes {
	probes := Probes{
		UVVersionWithLayout: func(ctx context.Context, _ *config.Layout, exePath string) (string, error) {
			return probeUVVersionWithLayout(ctx, layout, exePath, probeUVTimeout)
		},
		DiskFree:   probeDiskFree,
		FileSystem: filesystem.FileSystemName,
	}
	if runtime.GOOS != "windows" {
		probes.PythonExecutable = probeManagedPythonExecutable
	}
	return probes
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
func probeManagedPythonExecutable(ctx context.Context, layout *config.Layout) (string, error) {
	executable, err := layout.UVExecutable(uv.FixedVersion)
	if err != nil {
		return "", err
	}
	runner, err := uv.NewRunner(uv.RunnerConfig{Executable: executable, ProjectDir: layout.AppRoot(), PythonInstallDir: layout.PythonDir(), ProjectEnvDir: layout.VenvDir(), CacheDir: layout.UVCacheDir()})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, probeUVTimeout)
	defer cancel()
	result, err := runner.Run(ctx, []string{"--no-cache", "python", "find", "--managed-python", "--no-python-downloads", "--no-project"}, uv.RunOptions{Stage: protocol.StagePythonCheck, Environment: map[string]string{"UV_OFFLINE": "1"}})
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(result.Stdout)
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n") {
		return "", errors.New("managed python path is invalid")
	}
	return filepath.Clean(path), nil
}
