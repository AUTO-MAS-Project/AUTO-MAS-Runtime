//go:build linux || darwin

package lock_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/lock"
)

func TestPOSIXLock_Conflict(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	a, err := lock.NewSet(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	}()
	b, err := lock.NewSet(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	}()
	result, err := a.AcquireBackend(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.AcquireMutation(t.Context()); err == nil {
		t.Fatal("got mutation alongside backend, want conflict")
	}
	staged, err := b.AcquireStaging(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = staged.Lease().Close(); err != nil {
		t.Fatal(err)
	}
	if err = result.Lease().Close(); err != nil {
		t.Fatal(err)
	}
	result, err = b.AcquireMutation(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = result.Lease().Close(); err != nil {
		t.Fatal(err)
	}
}
func TestPOSIXLock_RejectReplacement(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	a, err := lock.NewSet(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	}()
	held, err := a.AcquireBackend(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// 同名文件替换无法替换根 inode 上的租约协调锚点。
	path := filepath.Join(root, "backend.lock")
	if err = os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	b, err := lock.NewSet(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err = b.AcquireBackend(t.Context()); err == nil {
		t.Fatal("got second backend lease after replacement, want conflict")
	}
	if err = held.Lease().Close(); err != nil {
		t.Fatal(err)
	}
	result, err := b.AcquireBackend(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = result.Lease().Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPOSIXLock_RejectSecondLeaseOnSameSet(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	set, err := lock.NewSet(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	held, err := set.AcquireBackend(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = set.AcquireStaging(t.Context()); err == nil {
		t.Fatal("got second lease on same set, want conflict")
	}
	if err = set.Close(); err != nil {
		t.Fatal(err)
	}
	if err = held.Lease().Close(); err != nil {
		t.Fatal(err)
	}
	other, err := lock.NewSet(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	result, err := other.AcquireBackend(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = result.Lease().Close(); err != nil {
		t.Fatal(err)
	}
	if err = other.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPOSIXLock_ExitHelper(t *testing.T) {
	if os.Getenv("M11_LOCK_HELPER_ROOT") == "" {
		return
	}
	root := os.Getenv("M11_LOCK_HELPER_ROOT")
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	set, err := lock.NewSet(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = set.AcquireBackend(t.Context()); err != nil {
		t.Fatal(err)
	}
	// 故意不 Close，进程退出后由下一实例基于内核身份恢复残留元数据。
	fmt.Println("ready")
}
func TestPOSIXLock_ProcessDeathRecoversLease(t *testing.T) {
	root := t.TempDir()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestPOSIXLock_ExitHelper$")
	command.Env = append(os.Environ(), "M11_LOCK_HELPER_ROOT="+root)
	if output, err := command.CombinedOutput(); err != nil || !strings.Contains(string(output), "ready") {
		t.Fatalf("helper %v %s", err, output)
	}
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	set, err := lock.NewSet(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	result, err := set.AcquireBackend(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Recovered() {
		t.Fatal("got fresh lease, want dead owner recovery")
	}
	if err = result.Lease().Close(); err != nil {
		t.Fatal(err)
	}
	if err = set.Close(); err != nil {
		t.Fatal(err)
	}
}
