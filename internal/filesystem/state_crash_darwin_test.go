//go:build darwin

package filesystem_test

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

// TestPOSIXState_CommittedWriterKilled 验证写入返回后进程被强杀不阻止后续读写。
// 该测试不覆盖写入中途的崩溃点或整机断电。
func TestPOSIXState_CommittedWriterKilled(t *testing.T) {
	if root := os.Getenv("AUTO_MAS_TEST_STATE_CRASH_ROOT"); root != "" {
		layout, err := config.NewLayout(root, root)
		if err != nil {
			t.Fatal(err)
		}
		files, err := filesystem.NewStateFiles(t.Context(), layout)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := files.WriteAtomic(t.Context(), filesystem.StateMutation, []byte("committed")); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(os.Stdout, "committed")
		// 父进程收到确认后强杀本进程，不主动关闭状态能力。
		_, _ = os.Stdin.Read(make([]byte, 1))
		return
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPOSIXState_CommittedWriterKilled$")
	cmd.Env = append(os.Environ(), "AUTO_MAS_TEST_STATE_CRASH_ROOT="+root)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "committed\n" {
		t.Fatalf("child confirmation = %q, %v; want committed", line, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed child exited successfully")
	}
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := filesystem.NewStateFiles(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	snapshot, err := files.Read(t.Context(), filesystem.StateMutation, 1024)
	if err != nil || string(snapshot.Bytes()) != "committed" {
		t.Fatalf("Read() = %q, %v; want committed", snapshot.Bytes(), err)
	}
	if result, err := files.WriteAtomic(t.Context(), filesystem.StateMutation, []byte("next")); err != nil || !result.MutationApplied {
		t.Fatalf("WriteAtomic() = %+v, %v; want applied", result, err)
	}
}
