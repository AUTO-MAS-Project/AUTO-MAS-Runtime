//go:build darwin

package filesystem

import (
	"context"
	"sync"
)

type darwinStateQueueKey struct {
	volume uint64
	fileID [16]byte
}

type darwinStateWaiter struct {
	key   darwinStateQueueKey
	ready chan struct{}
}

type darwinStateQueues struct {
	mu      sync.Mutex // 保护各目录队列及其交接顺序。
	entries map[darwinStateQueueKey][]*darwinStateWaiter
}

// 进程内所有 StateFiles 必须共享同一队列；跨进程互斥仍由 flock 保证。
var darwinStateTurns darwinStateQueues

func (q *darwinStateQueues) enqueue(key darwinStateQueueKey) *darwinStateWaiter {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.entries == nil {
		q.entries = make(map[darwinStateQueueKey][]*darwinStateWaiter)
	}
	w := &darwinStateWaiter{key: key, ready: make(chan struct{})}
	queue := q.entries[key]
	q.entries[key] = append(queue, w)
	if len(queue) == 0 {
		close(w.ready)
	}
	return w
}

func (q *darwinStateQueues) wait(ctx context.Context, w *darwinStateWaiter) error {
	select {
	case <-ctx.Done():
		q.release(w)
		return ctx.Err()
	case <-w.ready:
		if err := ctx.Err(); err != nil {
			q.release(w)
			return err
		}
		return nil
	}
}

func (q *darwinStateQueues) release(w *darwinStateWaiter) {
	q.mu.Lock()
	defer q.mu.Unlock()
	queue := q.entries[w.key]
	for i, item := range queue {
		if item != w {
			continue
		}
		queue = append(queue[:i], queue[i+1:]...)
		if len(queue) == 0 {
			delete(q.entries, w.key)
			return
		}
		q.entries[w.key] = queue
		if i == 0 {
			close(queue[0].ready)
		}
		return
	}
}
