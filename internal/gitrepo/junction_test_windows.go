package gitrepo

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

// mustCreateGitRepoJunction 创建测试用目录 Junction，并只在清理时移除链接本身。
func mustCreateGitRepoJunction(t *testing.T, link, target string) {
	t.Helper()
	script := `$ErrorActionPreference = 'Stop'
$target = $env:AUTO_MAS_GITREPO_JUNCTION_TARGET
$path = $env:AUTO_MAS_GITREPO_JUNCTION_PATH
if ([string]::IsNullOrWhiteSpace($target) -or
    [string]::IsNullOrWhiteSpace($path)) {
    throw 'gitrepo junction environment is incomplete'
}
try {
    New-Item -ItemType Junction -Path $path -Target $target -ErrorAction Stop |
        Out-Null
} catch {
    $exception = $_.Exception
    $nativeCode = $exception.HResult -band 0xffff
    $category = $_.CategoryInfo.Category
    $skippable = (
        $exception -is [System.UnauthorizedAccessException] -or
        $exception -is [System.PlatformNotSupportedException] -or
        $exception -is [System.NotSupportedException] -or
        $category -eq [System.Management.Automation.ErrorCategory]::PermissionDenied -or
        $category -eq [System.Management.Automation.ErrorCategory]::SecurityError -or
        $nativeCode -in 5, 50, 1314
    )
    if ($skippable) {
        exit 77
    }
    throw
}`
	command := exec.CommandContext(
		t.Context(),
		"pwsh",
		"-NoLogo",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		script,
	)
	command.Env = append(
		os.Environ(),
		"AUTO_MAS_GITREPO_JUNCTION_TARGET="+target,
		"AUTO_MAS_GITREPO_JUNCTION_PATH="+link,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 77 {
			t.Skipf("directory Junction unavailable: %v; output=%q", err, output)
		}
		t.Fatalf("create directory Junction: %v; output=%q", err, output)
	}
	t.Cleanup(func() {
		if err := os.Remove(link); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove Junction %q: %v", link, err)
		}
	})
}
