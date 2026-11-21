//go:build darwin

package filesystem

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestDarwinStateQueue_FIFOAndCancellation(t *testing.T) {
	var q darwinStateQueues
	key := darwinStateQueueKey{volume: 1, fileID: [16]byte{1}}
	first := q.enqueue(key)
	second := q.enqueue(key)
	third := q.enqueue(key)
	other := q.enqueue(darwinStateQueueKey{volume: 1, fileID: [16]byte{2}})
	if err := q.wait(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	q.release(other)
	if err := q.wait(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.wait(ctx, second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v", err)
	}
	select {
	case <-third.ready:
		t.Fatal("third entered before first released")
	default:
	}
	q.release(first)
	if err := q.wait(context.Background(), third); err != nil {
		t.Fatal(err)
	}
	q.release(third)
	if len(q.entries) != 0 {
		t.Fatalf("queue entries = %d, want 0", len(q.entries))
	}
}

func TestDarwinStateQueue_ExternalLockCancellationReleasesTurn(t *testing.T) {
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
	dir, err := darwinStateDirectory(t.Context(), layout, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(dir.file.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	_, writeErr := files.WriteAtomic(ctx, StateMutation, []byte("first"))
	cancel()
	if !errors.Is(writeErr, context.DeadlineExceeded) {
		t.Errorf("WriteAtomic() error = %v, want deadline exceeded", writeErr)
	}
	if err := unix.Flock(int(dir.file.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := dir.file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := files.WriteAtomic(t.Context(), StateMutation, []byte("second")); err != nil {
		t.Fatalf("WriteAtomic() after cancellation = %v", err)
	}
}

func TestDarwinStateQueue_CancelledHeadHandsOff(t *testing.T) {
	var q darwinStateQueues
	key := darwinStateQueueKey{volume: 2}
	first := q.enqueue(key)
	second := q.enqueue(key)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.wait(ctx, first); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled head wait = %v", err)
	}
	if err := q.wait(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	q.release(second)
	if len(q.entries) != 0 {
		t.Fatalf("queue entries = %d, want 0", len(q.entries))
	}
}
