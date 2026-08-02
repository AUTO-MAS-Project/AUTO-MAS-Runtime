package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestProbeDiskFree_Production(t *testing.T) {
	t.Parallel()
	probes := ProductionProbes()
	if probes.UVVersion == nil || probes.DiskFree == nil {
		t.Fatal("ProductionProbes() returned nil probe functions")
	}
	free, err := probes.DiskFree(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("DiskFree() error = %v", err)
	}
	if free == 0 {
		t.Error("DiskFree() = 0, want positive free bytes")
	}
}

func TestProbeDiskFree_RejectsEmptyPath(t *testing.T) {
	t.Parallel()
	probes := ProductionProbes()
	if _, err := probes.DiskFree(context.Background(), ""); err == nil {
		t.Fatal("DiskFree(\"\") error = nil, want error")
	}
}

// TestProbeUVVersion_TimesOut 证明 uv 版本探测在固定超时到期后返回错误，
// 且错误分类为稳定的 timeout，不会让 doctor 因挂死的 uv.exe 无法结束。
func TestProbeUVVersion_TimesOut(t *testing.T) {
	t.Parallel()
	helper := filepath.Join(t.TempDir(), "block.exe")
	buildBlockingHelper(t, helper)
	_, err := probeUVVersionWithTimeout(
		context.Background(),
		helper,
		500*time.Millisecond,
	)
	if err == nil {
		t.Fatal("probeUVVersionWithTimeout() error = nil, want timeout error")
	}
	if kind := errorKind(err); kind != "timeout" {
		t.Errorf("errorKind(err) = %q, want timeout (err=%v)", kind, err)
	}
}

// buildBlockingHelper 在测试临时目录内编译一个永久阻塞的可执行文件，
// 用于确定性地触发探测超时，不依赖真实 Python/uv/Git。
func buildBlockingHelper(t *testing.T, output string) {
	t.Helper()
	source := filepath.Join(filepath.Dir(output), "block.go")
	const sourceText = `package main

import "time"

func main() {
	// 长睡眠不会触发 Go 运行时死锁检测，进程保持存活直到被探测超时终止。
	time.Sleep(24 * time.Hour)
}
`
	if err := os.WriteFile(source, []byte(sourceText), 0o600); err != nil {
		t.Fatalf("WriteFile(helper source) error = %v", err)
	}
	command := exec.CommandContext(
		t.Context(),
		"go",
		"build",
		"-o", output,
		source,
	)
	if data, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go build blocking helper: %v; output=%q", err, data)
	}
}
