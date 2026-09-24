package uv

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// 生产装配必须给依赖服务接上能删 venv 的删除器；此前误用 uv 版本目录删除器，
// 删 venv 的请求不带 Version，`dependencies rebuild` 与顶层 `repair` 必然失败。
func TestProductionEnvironment_RebuildDependenciesRemovesManagedVenv(t *testing.T) {
	tests := []struct {
		name        string
		venvExists  bool
		wantRebuilt bool
	}{
		{name: "existing venv", venvExists: true, wantRebuilt: true},
		{name: "missing venv", venvExists: false, wantRebuilt: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			layout, err := config.NewLayout(root, filepath.Dir(root))
			if err != nil {
				t.Fatalf("NewLayout() error = %v", err)
			}
			if tt.venvExists {
				if err := os.MkdirAll(layout.VenvDir(), 0o700); err != nil {
					t.Fatalf("MkdirAll() error = %v", err)
				}
				if err := os.WriteFile(layout.VenvConfigFile(), []byte("home = x\n"), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
			}
			environment, err := NewProductionEnvironment(layout, WithProductionRelay(nil))
			if err != nil {
				t.Fatalf("NewProductionEnvironment() error = %v", err)
			}

			result, err := environment.RebuildDependencies(context.Background(), dependencyTestRequest(layout))
			if err != nil {
				t.Fatalf("RebuildDependencies() error = %v", err)
			}
			if result.Rebuilt != tt.wantRebuilt {
				t.Fatalf("RebuildDependencies() rebuilt = %t, want %t", result.Rebuilt, tt.wantRebuilt)
			}
			if _, err := os.Stat(layout.VenvDir()); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("venv stat error = %v, want not exist", err)
			}
		})
	}
}
