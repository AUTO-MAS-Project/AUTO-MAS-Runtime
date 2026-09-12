package uv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlanLock_RealDevLockTotalsWindowsArtifacts 用 upstream/dev@cca4ac486 的真实锁证明规划器
// 只计入 Windows 可安装制品：darwin / linux 专属包被排除，每项都带 sha256 与大小，总量落在实测区间。
func TestPlanLock_RealDevLockTotalsWindowsArtifacts(t *testing.T) {
	t.Parallel()

	lock := readLockFixture(t, "dev-cca4ac486.lock")
	plan, err := PlanLock(lock, PythonVersion{Major: 3, Minor: 12, Patch: 13})
	if err != nil {
		t.Fatalf("PlanLock() error = %v", err)
	}
	names := make(map[string]LockArtifact, len(plan.Items))
	for _, item := range plan.Items {
		if item.Size <= 0 || len(item.SHA256) != 64 || item.Path == "" || strings.HasPrefix(item.Path, "/") {
			t.Errorf("artifact %+v is incomplete", item)
		}
		names[item.Package] = item
	}
	for _, excluded := range []string{"pyobjc-core", "pyobjc-framework-quartz", "rubicon-objc", "python-xlib", "python3-xlib", "evdev", "pytest", "auto-mas"} {
		if _, ok := names[excluded]; ok {
			t.Errorf("%s is planned, want excluded on windows", excluded)
		}
	}
	for _, required := range []string{"opencv-python", "numpy", "pywin32", "keyboard", "socksio", "colorama", "maafw"} {
		if _, ok := names[required]; !ok {
			t.Errorf("%s is missing from the plan", required)
		}
	}
	if item := names["opencv-python"]; !strings.HasSuffix(item.Path, "win_amd64.whl") || item.Size < 30_000_000 {
		t.Errorf("opencv-python artifact = %+v, want the win_amd64 wheel", item)
	}
	if item := names["pywin32"]; !strings.Contains(item.Path, "cp312") || !strings.HasSuffix(item.Path, "win_amd64.whl") {
		t.Errorf("pywin32 artifact = %+v, want the cp312 win_amd64 wheel", item)
	}
	if plan.TotalBytes < 130_000_000 || plan.TotalBytes > 152_000_000 {
		t.Errorf("TotalBytes = %d, want within the measured 130~152 MB window", plan.TotalBytes)
	}
	if plan.Largest.Package != "opencv-python" {
		t.Errorf("Largest = %+v, want opencv-python", plan.Largest)
	}
	if plan.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0 for the dev lock", plan.Skipped)
	}
}

// TestPlanLock_UnknownMarkerCountsAsReachable 锁定失败关闭方向：求值不了的 marker 按可达处理并计数，只会高估。
func TestPlanLock_UnknownMarkerCountsAsReachable(t *testing.T) {
	t.Parallel()

	lock := lockHeader() + `
[[package]]
name = "root"
version = "0.0.1"
source = { virtual = "." }
dependencies = [
    { name = "alpha", marker = "platform_flavor == 'exotic'" },
    { name = "beta", marker = "sys_platform == 'darwin'" },
]

[[package]]
name = "alpha"
version = "1.0.0"
source = { registry = "https://pypi.org/simple" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/aa/alpha-1.0.0-py3-none-any.whl", hash = "sha256:` + strings.Repeat("a", 64) + `", size = 10 },
]

[[package]]
name = "beta"
version = "1.0.0"
source = { registry = "https://pypi.org/simple" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/bb/beta-1.0.0-py3-none-any.whl", hash = "sha256:` + strings.Repeat("b", 64) + `", size = 20 },
]
`
	plan, err := PlanLock(lock, PythonVersion{Major: 3, Minor: 12, Patch: 13})
	if err != nil {
		t.Fatalf("PlanLock() error = %v", err)
	}
	if len(plan.Items) != 1 || plan.Items[0].Package != "alpha" {
		t.Fatalf("Items = %+v, want only alpha", plan.Items)
	}
	if plan.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", plan.Skipped)
	}
	if plan.TotalBytes != 10 {
		t.Errorf("TotalBytes = %d, want 10", plan.TotalBytes)
	}
	if plan.Items[0].Path != "aa/alpha-1.0.0-py3-none-any.whl" {
		t.Errorf("Path = %q, want PyPI relative path", plan.Items[0].Path)
	}
}

// TestPlanLock_ExtrasPullOptionalDependencies 锁定 extra 边会把目标包对应的 optional-dependencies 一并规划进来。
func TestPlanLock_ExtrasPullOptionalDependencies(t *testing.T) {
	t.Parallel()

	lock := lockHeader() + `
[[package]]
name = "root"
version = "0.0.1"
source = { virtual = "." }
dependencies = [
    { name = "client", extra = ["socks"] },
]

[[package]]
name = "client"
version = "1.0.0"
source = { registry = "https://pypi.org/simple" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/cc/client-1.0.0-py3-none-any.whl", hash = "sha256:` + strings.Repeat("c", 64) + `", size = 30 },
]

[package.optional-dependencies]
socks = [
    { name = "socksio" },
]
http2 = [
    { name = "h2" },
]

[[package]]
name = "socksio"
version = "1.0.0"
source = { registry = "https://pypi.org/simple" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/dd/socksio-1.0.0-py3-none-any.whl", hash = "sha256:` + strings.Repeat("d", 64) + `", size = 40 },
]

[[package]]
name = "h2"
version = "1.0.0"
source = { registry = "https://pypi.org/simple" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/ee/h2-1.0.0-py3-none-any.whl", hash = "sha256:` + strings.Repeat("e", 64) + `", size = 50 },
]
`
	plan, err := PlanLock(lock, PythonVersion{Major: 3, Minor: 12, Patch: 13})
	if err != nil {
		t.Fatalf("PlanLock() error = %v", err)
	}
	got := make(map[string]bool, len(plan.Items))
	for _, item := range plan.Items {
		got[item.Package] = true
	}
	if !got["client"] || !got["socksio"] || got["h2"] {
		t.Errorf("planned packages = %v, want client + socksio without h2", got)
	}
	if plan.TotalBytes != 70 {
		t.Errorf("TotalBytes = %d, want 70", plan.TotalBytes)
	}
}

// TestPlanLock_WheelSelection 锁定本平台 wheel 的优先级：cp312 > abi3 > py3 win_amd64 > none-any > sdist。
func TestPlanLock_WheelSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files []string
		want  string
	}{
		{"cp312 beats abi3", []string{"x-1-cp37-abi3-win_amd64.whl", "x-1-cp312-cp312-win_amd64.whl", "x-1-py3-none-any.whl"}, "x-1-cp312-cp312-win_amd64.whl"},
		{"abi3 beats any", []string{"x-1-py3-none-any.whl", "x-1-cp37-abi3-win_amd64.whl"}, "x-1-cp37-abi3-win_amd64.whl"},
		{"compressed tags", []string{"x-1-py2.py3-none-any.whl"}, "x-1-py2.py3-none-any.whl"},
		{"ignore other platforms", []string{"x-1-cp312-cp312-manylinux_2_17_x86_64.whl", "x-1-cp312-cp312-macosx_11_0_arm64.whl", "x-1-py3-none-any.whl"}, "x-1-py3-none-any.whl"},
		{"ignore other pythons", []string{"x-1-cp311-cp311-win_amd64.whl", "x-1-cp313-cp313-win_amd64.whl", "x-1-py3-none-win_amd64.whl"}, "x-1-py3-none-win_amd64.whl"},
		{"ignore win32", []string{"x-1-cp312-cp312-win32.whl", "x-1-py3-none-any.whl"}, "x-1-py3-none-any.whl"},
		{"sdist fallback", []string{"x-1-cp311-cp311-win_amd64.whl"}, "x-1.tar.gz"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var builder strings.Builder
			builder.WriteString(lockHeader())
			builder.WriteString("\n[[package]]\nname = \"root\"\nversion = \"0.0.1\"\nsource = { virtual = \".\" }\ndependencies = [\n    { name = \"x\" },\n]\n\n")
			builder.WriteString("[[package]]\nname = \"x\"\nversion = \"1\"\nsource = { registry = \"https://pypi.org/simple\" }\n")
			builder.WriteString("sdist = { url = \"https://files.pythonhosted.org/packages/sd/x-1.tar.gz\", hash = \"sha256:" + strings.Repeat("c", 64) + "\", size = 5 }\nwheels = [\n")
			for _, file := range test.files {
				builder.WriteString("    { url = \"https://files.pythonhosted.org/packages/wh/" + file + "\", hash = \"sha256:" + strings.Repeat("d", 64) + "\", size = 7 },\n")
			}
			builder.WriteString("]\n")
			plan, err := PlanLock(builder.String(), PythonVersion{Major: 3, Minor: 12, Patch: 13})
			if err != nil {
				t.Fatalf("PlanLock() error = %v", err)
			}
			if len(plan.Items) != 1 {
				t.Fatalf("Items = %+v, want one", plan.Items)
			}
			if got := filepath.Base(plan.Items[0].Path); got != test.want {
				t.Errorf("selected %q, want %q", got, test.want)
			}
		})
	}
}

// TestPlanLock_RejectsUnparsableLock 锁定解析失败返回 error，由调用方降级为无总量。
func TestPlanLock_RejectsUnparsableLock(t *testing.T) {
	t.Parallel()

	version := PythonVersion{Major: 3, Minor: 12, Patch: 13}
	if _, err := PlanLock("this is not toml = = =", version); err == nil {
		t.Fatal("PlanLock(garbage) error = nil, want error")
	}
	if _, err := PlanLock("version = 1\n", version); err == nil {
		t.Fatal("PlanLock(no root) error = nil, want error")
	}
}

func lockHeader() string {
	return "version = 1\nrevision = 3\nrequires-python = \"==3.12.*\"\n"
}

func readLockFixture(t *testing.T, name string) string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join("testdata", "uvlock", name))
	if err != nil {
		t.Fatalf("read lock fixture %s: %v", name, err)
	}
	return string(content)
}
