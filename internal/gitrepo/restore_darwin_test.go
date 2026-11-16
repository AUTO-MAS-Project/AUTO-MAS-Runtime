package gitrepo

import (
	"errors"
	"os"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

func TestDarwinSourceRestore_RejectsChangedDestination(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "replaced file"
		if missing {
			name = "new file at missing path"
		}
		t.Run(name, func(t *testing.T) {
			layout, commit := sourceRestoreFixture(t)
			target := layout.UVLockFile()
			if missing {
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(target, []byte("broken"), 0o600); err != nil {
				t.Fatal(err)
			}
			called := false
			count, err := restoreTestRepository(t.Context(), t, layout, commit, func() error {
				called = true
				if !missing {
					if err := os.Rename(target, target+".saved"); err != nil {
						return err
					}
				}
				return os.WriteFile(target, []byte("replacement"), 0o600)
			})
			if !called || count != 0 || !errors.Is(err, filesystem.ErrIdentityChanged) {
				t.Fatalf("Restore = %d, %v, barrier=%t; want 0, identity changed, barrier=true", count, err, called)
			}
			if data, err := os.ReadFile(target); err != nil || string(data) != "replacement" {
				t.Fatalf("replacement = %q, %v, want unchanged", data, err)
			}
		})
	}
}
