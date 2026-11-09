package lock

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestDarwinLock_ConcurrentCrossAcquire(t *testing.T) {
	layout := darwinLockLayout(t)
	sets := [2]*Set{darwinTestSet(t, layout), darwinTestSet(t, layout)}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	type outcome struct {
		kind   Kind
		result AcquisitionResult
		err    error
	}
	results := make(chan outcome, 2)
	var workers sync.WaitGroup
	workers.Go(func() {
		<-start
		got, err := sets[0].AcquireBackend(ctx)
		results <- outcome{kind: KindBackend, result: got, err: err}
	})
	workers.Go(func() {
		<-start
		got, err := sets[1].AcquireMutation(ctx)
		results <- outcome{kind: KindMutation, result: got, err: err}
	})
	close(start)
	workers.Wait()
	first, second := <-results, <-results
	winner, loser := first, second
	if winner.err != nil {
		winner, loser = second, first
	}
	if winner.err != nil || winner.result.Lease() == nil || loser.result.Lease() != nil {
		t.Fatalf("cross acquire = %#v and %#v, want exactly one owner", first, second)
	}
	want := protocol.CodeBackendStillRunning
	if winner.kind == KindMutation {
		want = protocol.CodeMutationInProgress
	}
	assertDarwinLockConflict(t, loser.err, want)
}

func TestDarwinLock_RejectsReplacedObjects(t *testing.T) {
	for _, scenario := range []string{"backend file", "missing observed leaf", "state directory", "missing coordinator", "link", "hard link", "nonempty file"} {
		t.Run(scenario, func(t *testing.T) {
			layout := darwinLockLayout(t)
			paths := layout.DarwinLockPaths()
			set := darwinTestSet(t, layout)
			first, err := set.AcquireBackend(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "missing coordinator" {
				if err := os.Rename(paths.Coordinator, paths.Coordinator+".old"); err != nil {
					t.Fatal(err)
				}
				other := darwinTestSet(t, layout)
				if result, err := other.AcquireMutation(t.Context()); result.Lease() != nil || !errors.Is(err, filesystem.ErrIdentityChanged) {
					t.Fatalf("AcquireMutation with missing gate = %#v, %v, want identity error", result, err)
				}
				if _, err := os.Lstat(paths.Coordinator); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("coordinator recreated: %v", err)
				}
				return
			}
			if err := first.Lease().Close(); err != nil {
				t.Fatal(err)
			}
			if scenario == "state directory" {
				if err := os.Rename(layout.StateDir(), layout.StateDir()+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(layout.StateDir(), 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(paths.Backend, paths.Backend+".old"); err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "backend file":
					err = os.WriteFile(paths.Backend, nil, 0o600)
				case "link":
					err = os.Symlink(paths.Backend+".old", paths.Backend)
				case "hard link":
					err = os.Link(paths.Backend+".old", paths.Backend)
				case "nonempty file":
					err = os.WriteFile(paths.Backend, []byte("preserve"), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if result, err := set.AcquireBackend(t.Context()); result.Lease() != nil || !errors.Is(err, filesystem.ErrIdentityChanged) {
				t.Fatalf("AcquireBackend with %s = %#v, %v, want identity error", scenario, result, err)
			}
			if _, err := set.Probe(t.Context(), KindMutation); !errors.Is(err, ErrPoisoned) {
				t.Fatalf("Probe after identity failure = %v, want poisoned set", err)
			}
			if scenario == "missing observed leaf" {
				if _, err := os.Lstat(paths.Backend); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("missing observed leaf recreated: %v", err)
				}
			}
			if scenario == "nonempty file" {
				if payload, err := os.ReadFile(paths.Backend); err != nil || string(payload) != "preserve" {
					t.Fatalf("unknown file = %q, %v, want preserved", payload, err)
				}
			}
		})
	}
}

func TestDarwinLock_ReadOnlyProbeAndCancel(t *testing.T) {
	layout := darwinLockLayout(t)
	set := darwinTestSet(t, layout)
	if status, err := set.Probe(t.Context(), KindBackend); err != nil || status.Held || status.Recovered {
		t.Fatalf("Probe fresh root = %#v, %v, want free", status, err)
	}
	if _, err := os.Stat(layout.StateDir()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only probe created state directory: %v", err)
	}
	if err := os.Mkdir(layout.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if status, err := set.Probe(t.Context(), KindMutation); err != nil || status.Held {
		t.Fatalf("Probe empty state directory = %#v, %v, want free", status, err)
	}
	if files, err := os.ReadDir(layout.StateDir()); err != nil || len(files) != 0 {
		t.Fatalf("read-only probe changed state directory: %v, %v", files, err)
	}

	gate, err := os.OpenFile(layout.DarwinLockPaths().Coordinator, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := gate.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := unix.Flock(int(gate.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if result, err := set.AcquireBackend(ctx); result.Lease() != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AcquireBackend while gate is held = %#v, %v, want canceled wait", result, err)
	}
	if _, err := os.Lstat(layout.DarwinLockPaths().Backend); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled acquire created a business lock: %v", err)
	}
	if err := unix.Flock(int(gate.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if result, err := set.AcquireBackend(t.Context()); err != nil || result.Lease() == nil {
		t.Fatalf("AcquireBackend after cancel = %#v, %v, want lease", result, err)
	}
}

const darwinHolderRoot = "AUTO_MAS_DARWIN_LOCK_HOLDER_ROOT"

func TestDarwinLock_HolderCrash(t *testing.T) {
	if root := os.Getenv(darwinHolderRoot); root != "" {
		runDarwinLockHolder(t, root)
		return
	}
	layout := darwinLockLayout(t)
	observer := darwinTestSet(t, layout)
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, bin, "-test.run=^TestDarwinLock_HolderCrash$")
	child.Env = append(os.Environ(), darwinHolderRoot+"="+layout.AppRoot())
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(errors.Join(err, stdin.Close()))
	}
	if err := child.Start(); err != nil {
		t.Fatal(errors.Join(err, stdin.Close(), stdout.Close()))
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			if err := child.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error(err)
			}
			var exit *exec.ExitError
			if err := child.Wait(); err != nil && !errors.As(err, &exit) {
				t.Error(err)
			}
		}
	})
	ready := bufio.NewScanner(stdout)
	if !ready.Scan() || ready.Text() != "locked" {
		t.Fatalf("child readiness = %q, %v", ready.Text(), ready.Err())
	}
	path := layout.DarwinLockPaths().Backend
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := observer.Probe(t.Context(), KindBackend); err != nil || !status.Held {
		t.Fatalf("Probe before kill = %#v, %v, want held", status, err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitErr := child.Wait()
	reaped = true
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) {
		t.Fatalf("child Wait = %v, want process killed", waitErr)
	}
	result, err := observer.AcquireBackend(t.Context())
	if err != nil || result.Lease() == nil || result.Recovered() {
		t.Fatalf("AcquireBackend after kill = %#v, %v, want normal lease", result, err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("lock identity after crash = %v, %v, want same object", after, err)
	}
}

func runDarwinLockHolder(t *testing.T, root string) {
	t.Helper()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	set := darwinTestSet(t, layout)
	if result, err := set.AcquireBackend(t.Context()); err != nil || result.Lease() == nil {
		t.Fatalf("child AcquireBackend = %#v, %v", result, err)
	}
	if _, err := os.Stdout.WriteString("locked\n"); err != nil {
		t.Fatal(err)
	}
	// 只有父进程杀死子进程或关闭管道，才会结束持锁。
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}
