//go:build windows

package lock

import (
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestSet_StagingAllowsBackendAndExcludesMutations(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	newSet := func() *Set {
		s, err := NewSet(t.Context(), layout)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		return s
	}
	backend := newSet()
	b, err := backend.AcquireBackend(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stager := newSet()
	s, err := stager.AcquireStaging(t.Context())
	if err != nil {
		t.Fatalf("AcquireStaging with backend = %v, want success", err)
	}
	contender := newSet()
	_, err = contender.AcquireStaging(t.Context())
	assertErrorCode(t, err, protocol.CodeMutationInProgress)
	_, err = contender.AcquireMutation(t.Context())
	assertErrorCode(t, err, protocol.CodeMutationInProgress)
	if err := s.Lease().Close(); err != nil {
		t.Fatal(err)
	}
	_, err = contender.AcquireMutation(t.Context())
	assertErrorCode(t, err, protocol.CodeBackendStillRunning)
	if err := b.Lease().Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := contender.AcquireMutation(t.Context()); err != nil {
		t.Fatal(err)
	}
}
