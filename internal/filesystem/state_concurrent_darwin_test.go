//go:build darwin

package filesystem

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// TestPOSIXState_ConcurrentWriters 在原始并发写入路径上记录失败轮次的锁时间线。
func TestPOSIXState_ConcurrentWriters(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	type event struct {
		worker, iteration int
		name              string
		at                time.Duration
	}
	start := time.Now()
	var eventMu sync.Mutex
	var events []event
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			files, err := NewStateFiles(t.Context(), layout)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() {
				if err := files.Close(); err != nil {
					t.Error(err)
				}
			}()
			iteration := 0
			files.trace = func(name string) {
				at := time.Since(start)
				eventMu.Lock()
				events = append(events, event{worker: i, iteration: iteration, name: name, at: at})
				eventMu.Unlock()
			}
			for iteration = 0; iteration < 10; iteration++ {
				if _, err := files.WriteAtomic(t.Context(), StateMutation, bytes.Repeat([]byte("x"), 8192)); err != nil {
					t.Errorf("writer %d iteration %d: %v", i, iteration, err)
					return
				}
			}
		})
	}
	wg.Wait()
	data, err := os.ReadFile(layout.MutationStateFile())
	if err != nil || !bytes.Equal(data, bytes.Repeat([]byte("x"), 8192)) {
		t.Errorf("got length %d, %v", len(data), err)
	}
	if t.Failed() {
		for _, e := range events {
			t.Logf("elapsed=%v writer=%d iteration=%d event=%s", e.at, e.worker, e.iteration, e.name)
		}
	}
}

func TestDarwinState_WriteTimingEvents(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := NewStateFiles(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	var events []string
	files.trace = func(name string) { events = append(events, name) }
	if _, err := files.WriteAtomic(t.Context(), StateMutation, []byte("state")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"wait", "acquired", "write-finished", "released"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestDarwinState_WriteTimingRecordsWaitFailure(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := NewStateFiles(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := holder.Close(); err != nil {
			t.Error(err)
		}
	})
	waiter, err := NewStateFiles(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := waiter.Close(); err != nil {
			t.Error(err)
		}
	})
	dir, _, err := holder.open(t.Context(), StateMutation)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeDarwinStateDirectory(dir); err != nil {
			t.Error(err)
		}
	})
	var events []string
	waiter.trace = func(name string) { events = append(events, name) }
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := waiter.WriteAtomic(ctx, StateMutation, []byte("state")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WriteAtomic() error = %v, want deadline exceeded", err)
	}
	if want := []string{"wait", "wait-failed"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}
