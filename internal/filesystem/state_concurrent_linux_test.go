//go:build linux

package filesystem_test

import (
	"bytes"
	"os"
	"sync"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

func TestPOSIXState_ConcurrentWriters(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			files, err := filesystem.NewStateFiles(t.Context(), layout)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() {
				if err := files.Close(); err != nil {
					t.Error(err)
				}
			}()
			for j := 0; j < 10; j++ {
				if _, err := files.WriteAtomic(t.Context(), filesystem.StateMutation, bytes.Repeat([]byte("x"), 8192)); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	data, err := os.ReadFile(layout.MutationStateFile())
	if err != nil || !bytes.Equal(data, bytes.Repeat([]byte("x"), 8192)) {
		t.Fatalf("got length %d, %v", len(data), err)
	}
}
