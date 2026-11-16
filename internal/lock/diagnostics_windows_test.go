//go:build windows

package lock

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestNewSet_FailureDiagnostics(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	api := newTestWindowsAPI()
	api.createMutexErr[1] = windows.ERROR_ACCESS_DENIED
	set, err := newSet(t.Context(), layout, api)
	if set != nil {
		_ = set.Close()
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("newSet() error = %v, want access denied", err)
	}
	var diagnostic interface{ DiagnosticDetails() map[string]any }
	if !errors.As(err, &diagnostic) {
		t.Fatalf("error = %v, want structured diagnostic facts", err)
	}
	want := map[string]any{"mutex": map[string]any{
		"operation": "create-mutex", "component": "backend", "windowsError": uint64(5),
	}}
	if got := diagnostic.DiagnosticDetails(); !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostics = %#v, want %#v", got, want)
	}
}
