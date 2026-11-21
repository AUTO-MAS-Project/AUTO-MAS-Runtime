//go:build darwin

package filesystem

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// TestStateContention_DarwinTimings 分别记录等锁和持锁写入耗时，辅助定位慢卷与竞争。
func TestStateContention_DarwinTimings(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	files := make([]*StateFiles, 8)
	for i := range files {
		files[i], err = NewStateFiles(t.Context(), layout)
		if err != nil {
			t.Fatal(err)
		}
		f := files[i]
		t.Cleanup(func() {
			if err := f.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	type timing struct {
		wait, hold time.Duration
		err        error
	}
	results := make(chan timing, len(files)*10)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, f := range files {
		wg.Go(func() {
			<-start
			for range 10 {
				before := time.Now()
				dir, name, err := f.open(t.Context(), StateMutation)
				wait := time.Since(before)
				if err != nil {
					results <- timing{wait: wait, err: err}
					return
				}
				before = time.Now()
				_, err = replaceDarwin(t.Context(), dir, name, bytes.Repeat([]byte("x"), 8192))
				hold := time.Since(before)
				closeErr := closeDarwinStateDirectory(dir)
				if err == nil {
					err = closeErr
				}
				results <- timing{wait: wait, hold: hold, err: err}
				if err != nil {
					return
				}
			}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var maxWait, maxHold, totalHold time.Duration
	var count int
	for result := range results {
		count++
		maxWait = max(maxWait, result.wait)
		maxHold = max(maxHold, result.hold)
		totalHold += result.hold
		if result.err != nil {
			t.Errorf("wait=%v hold=%v error=%v", result.wait, result.hold, result.err)
		}
	}
	t.Logf("operations=%d max-wait=%v max-hold=%v total-hold=%v", count, maxWait, maxHold, totalHold)
	if count != len(files)*10 {
		t.Errorf("operations=%d, want %d", count, len(files)*10)
	}
}
