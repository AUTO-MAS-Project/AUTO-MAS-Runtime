package lock

import (
	"errors"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestDarwinLock_StagingMatrix(t *testing.T) {
	layout := darwinLockLayout(t)
	backend := darwinTestSet(t, layout)
	writer := darwinTestSet(t, layout)
	observer := darwinTestSet(t, layout)

	start, err := backend.AcquireBackend(t.Context())
	if err != nil || start.Lease() == nil || start.Recovered() {
		t.Fatalf("AcquireBackend = %#v, %v, want a normal lease", start, err)
	}
	_, err = observer.AcquireBackend(t.Context())
	assertDarwinLockConflict(t, err, protocol.CodeBackendAlreadyRunning)
	_, err = writer.AcquireMutation(t.Context())
	assertDarwinLockConflict(t, err, protocol.CodeBackendStillRunning)
	_, err = backend.AcquireStaging(t.Context())
	assertDarwinLockConflict(t, err, protocol.CodeBackendStillRunning)

	stage, err := writer.AcquireStaging(t.Context())
	if err != nil || stage.Lease() == nil || !stage.PeerProbe().Held || stage.PeerProbe().Recovered {
		t.Fatalf("AcquireStaging = %#v, %v, want a lease and a held backend", stage, err)
	}
	_, err = observer.AcquireStaging(t.Context())
	assertDarwinLockConflict(t, err, protocol.CodeMutationInProgress)
	if err := start.Lease().Close(); err != nil {
		t.Fatal(err)
	}
	_, err = observer.AcquireBackend(t.Context())
	assertDarwinLockConflict(t, err, protocol.CodeMutationInProgress)
	if err := stage.Lease().Close(); err != nil {
		t.Fatal(err)
	}

	if result, err := observer.AcquireMutation(t.Context()); err != nil || result.Lease() == nil || result.PeerProbe().Held {
		t.Fatalf("AcquireMutation after release = %#v, %v, want a lease and free backend", result, err)
	}
}

func darwinLockLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	return layout
}

func darwinTestSet(t *testing.T, layout *config.Layout) *Set {
	t.Helper()
	set, err := NewSet(t.Context(), layout)
	if err != nil {
		t.Fatalf("NewSet = %v, want a lock set", err)
	}
	t.Cleanup(func() {
		if err := set.Close(); err != nil {
			t.Error(err)
		}
	})
	return set
}

func assertDarwinLockConflict(t *testing.T, err error, code protocol.Code) {
	t.Helper()
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Code() != code {
		t.Fatalf("lock conflict = %v, want %s", err, code)
	}
}
