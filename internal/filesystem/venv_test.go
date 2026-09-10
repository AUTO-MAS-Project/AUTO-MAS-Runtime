package filesystem_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

// newVenvLayout 建一个只有 app-root 的布局，venv 内容由各用例自己铺。
func newVenvLayout(t *testing.T) *config.Layout {
	t.Helper()
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("NewLayout: %v", err)
	}
	return layout
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func TestInspectVenv_IntactWhenConfigAndInterpreterExist(t *testing.T) {
	layout := newVenvLayout(t)
	writeFile(t, layout.VenvConfigFile())
	writeFile(t, layout.VenvPythonExecutable())

	report := filesystem.InspectVenv(layout)

	if !report.Intact {
		t.Fatalf("Intact = false, want true; missing=%v", report.Missing)
	}
	if len(report.Missing) != 0 {
		t.Fatalf("Missing = %v, want empty", report.Missing)
	}
}

// 这是真机上出现过的残骸形态：目录和 python.exe 都在，只有 pyvenv.cfg 被删掉，
// CPython 因此以 `No pyvenv.cfg file` 立即退出。只查目录存在的判据看不出问题。
func TestInspectVenv_MissingPyvenvCfgIsNotIntact(t *testing.T) {
	layout := newVenvLayout(t)
	writeFile(t, layout.VenvPythonExecutable())

	report := filesystem.InspectVenv(layout)

	if report.Intact {
		t.Fatal("Intact = true, want false when pyvenv.cfg is absent")
	}
	if len(report.Missing) != 1 || report.Missing[0] != layout.VenvConfigFile() {
		t.Fatalf("Missing = %v, want only %s", report.Missing, layout.VenvConfigFile())
	}
}

func TestInspectVenv_MissingInterpreterIsNotIntact(t *testing.T) {
	layout := newVenvLayout(t)
	writeFile(t, layout.VenvConfigFile())

	report := filesystem.InspectVenv(layout)

	if report.Intact {
		t.Fatal("Intact = true, want false when the interpreter is absent")
	}
	if len(report.Missing) != 1 || report.Missing[0] != layout.VenvPythonExecutable() {
		t.Fatalf("Missing = %v, want only %s", report.Missing, layout.VenvPythonExecutable())
	}
}

func TestInspectVenv_AbsentVenvReportsBothPaths(t *testing.T) {
	layout := newVenvLayout(t)

	report := filesystem.InspectVenv(layout)

	if report.Intact {
		t.Fatal("Intact = true, want false for an absent venv")
	}
	if len(report.Missing) != 2 {
		t.Fatalf("Missing = %v, want both paths", report.Missing)
	}
}

// 目录占位不算解释器：真机残骸里 python.exe 被占用删不掉，但换个场景可能留下同名目录。
func TestInspectVenv_DirectoryInPlaceOfInterpreterIsNotIntact(t *testing.T) {
	layout := newVenvLayout(t)
	writeFile(t, layout.VenvConfigFile())
	if err := os.MkdirAll(layout.VenvPythonExecutable(), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	report := filesystem.InspectVenv(layout)

	if report.Intact {
		t.Fatal("Intact = true, want false when the interpreter path is a directory")
	}
}

func TestInspectVenv_NilLayoutIsNotIntact(t *testing.T) {
	report := filesystem.InspectVenv(nil)

	if report.Intact {
		t.Fatal("Intact = true, want false for a nil layout")
	}
}
