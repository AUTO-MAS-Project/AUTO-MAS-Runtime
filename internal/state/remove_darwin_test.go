//go:build darwin

package state

import (
	"errors"
	"sync"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestDarwinStore_RemoveTransactionAlreadyMissing(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := store.WriteTransaction(t.Context(), TransactionUpdate, validTransactionState(TransactionUpdate)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadTransaction(t.Context(), TransactionUpdate)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveTransaction(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveTransaction(t.Context(), snapshot); err != nil {
		t.Fatalf("second RemoveTransaction() = %v, want nil", err)
	}
	newState := validTransactionState(TransactionUpdate)
	newState.TargetVersion = "v5.4.0-new"
	if err := store.WriteTransaction(t.Context(), TransactionUpdate, newState); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveTransaction(t.Context(), snapshot); !errors.Is(err, ErrTransactionChanged) {
		t.Fatalf("RemoveTransaction(replaced) = %v, want ErrTransactionChanged", err)
	}
	current, err := store.ReadTransaction(t.Context(), TransactionUpdate)
	if err != nil || current.State() != newState {
		t.Fatalf("ReadTransaction() = %+v, %v, want replacement", current.State(), err)
	}
}

func TestDarwinStore_ConcurrentWritesAndRead(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	const writers = 8
	stores := make([]*Store, writers)
	for i := range stores {
		stores[i], err = NewStore(t.Context(), layout)
		if err != nil {
			t.Fatal(err)
		}
		store := stores[i]
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, store := range stores {
		wg.Go(func() {
			<-start
			if err := store.WriteTransaction(t.Context(), TransactionUpdate, validTransactionState(TransactionUpdate)); err != nil {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	if _, err := stores[0].ReadTransaction(t.Context(), TransactionUpdate); err != nil {
		t.Fatalf("ReadTransaction() = %v", err)
	}
}
