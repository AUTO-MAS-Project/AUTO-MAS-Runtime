package macosprobe

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func runRootRenamePeer(t *testing.T, source, destination string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, executable, "-test.run=^TestRoot_RenamePeer$", "--", "rename-peer", source, destination).CombinedOutput()
	if err != nil {
		t.Fatalf("same-user rename peer: %v: %s", err, output)
	}
}

// TestRoot_RenamePeer 只在父测试持有目录锁后以独立同用户进程执行重命名。
func TestRoot_RenamePeer(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--" {
			continue
		}
		args := os.Args[i+1:]
		if len(args) != 3 || args[0] != "rename-peer" {
			t.Fatal("invalid rename peer arguments")
		}
		if err := os.Rename(args[1], args[2]); err != nil {
			t.Fatal(err)
		}
		return
	}
}
