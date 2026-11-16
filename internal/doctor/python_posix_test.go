//go:build linux || darwin

package doctor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

func TestPOSIXDoctor_InstalledPythonLayout(t *testing.T) {
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(layout.PythonDir(), "cpython-3.12.13-linux-x86_64-gnu", "bin", "python3.12")
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	service := &Service{layout: layout, probes: Probes{PythonExecutable: func(context.Context, *config.Layout) (string, error) { return path, nil }}}
	if check := service.checkPython(t.Context()); check.Status != StatusOK {
		t.Fatalf("got %+v, want installed uv distribution accepted", check)
	}
}
