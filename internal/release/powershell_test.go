package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestWorkflowAndScripts_PowerShellAST 用 pwsh 的语言解析器校验全部工作流 run block
// 与 scripts/*.ps1 的 PowerShell 语法，恢复 T7.2 时代的手工 AST 门禁并以代码固化。
func TestWorkflowAndScripts_PowerShellAST(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell AST gate requires pwsh, which this gate only provisions on Windows runners")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() did not return the test file")
	}
	repoRoot := filepath.Join(filepath.Dir(file), "..", "..")

	workflows, err := filepath.Glob(filepath.Join(repoRoot, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatalf("Glob(workflows) error = %v", err)
	}
	if len(workflows) == 0 {
		t.Fatal("no workflow files found")
	}
	scripts, err := filepath.Glob(filepath.Join(repoRoot, "scripts", "*.ps1"))
	if err != nil {
		t.Fatalf("Glob(scripts) error = %v", err)
	}

	chunks := make(map[string]string)
	for _, workflow := range workflows {
		data, err := os.ReadFile(workflow)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", workflow, err)
		}
		for i, block := range extractRunBlocks(string(data)) {
			name := filepath.Base(workflow) + "#run[" + itoa(i) + "]"
			chunks[name] = block
		}
	}
	for _, script := range scripts {
		data, err := os.ReadFile(script)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", script, err)
		}
		chunks[filepath.Base(script)] = string(data)
	}
	if len(scripts) == 0 {
		t.Fatal("no PowerShell scripts found under scripts/")
	}

	// 逐块写临时文件，让 pwsh Parser.ParseFile 报告真实行号与错误。
	dir := t.TempDir()
	manifest := make([]string, 0, len(chunks))
	for name, source := range chunks {
		path := filepath.Join(dir, sanitizeFileName(name)+".ps1")
		if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", path, err)
		}
		manifest = append(manifest, name+"\t"+path)
	}
	manifestPath := filepath.Join(dir, "manifest.tsv")
	if err := os.WriteFile(manifestPath, []byte(strings.Join(manifest, "\n")), 0o644); err != nil {
		t.Fatalf("WriteFile(manifest) error = %v", err)
	}

	script := `
$ErrorActionPreference = "Stop"
$failures = @()
Get-Content -LiteralPath $args[0] -Encoding utf8 | ForEach-Object {
    $parts = $_ -split "` + "`t" + `", 2
    $name = $parts[0]
    $path = $parts[1]
    $tokens = $null
    $errors = $null
    [void][System.Management.Automation.Language.Parser]::ParseFile($path, [ref]$tokens, [ref]$errors)
    foreach ($parseError in $errors) {
        $failures += "$name : $($parseError.Extent.StartLineNumber):$($parseError.Extent.StartColumnNumber) $($parseError.Message)"
    }
}
if ($failures.Count -gt 0) {
    $failures | ForEach-Object { Write-Output "AST-FAIL $_" }
    exit 1
}
exit 0
`
	scriptPath := filepath.Join(dir, "ast-gate.ps1")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatalf("WriteFile(ast-gate) error = %v", err)
	}

	output, exitErr := runPwsh(t, scriptPath, manifestPath)
	if exitErr != nil {
		t.Fatalf("PowerShell AST gate failed:\n%s", output)
	}
}

// runPwsh 执行 pwsh 脚本并返回合并后的输出与错误（退出码非 0 时非 nil）。
func runPwsh(t *testing.T, scriptPath string, args ...string) (string, error) {
	t.Helper()
	cmdArgs := append([]string{"-NoProfile", "-NonInteractive", "-File", scriptPath}, args...)
	cmd := exec.Command("pwsh", cmdArgs...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// extractRunBlocks 提取 workflow YAML 中每个多行 run: block 的字面内容。
// 只处理块状标量（run: | 或 run: >），发布工作流不使用单行 run。
func extractRunBlocks(source string) []string {
	lines := strings.Split(source, "\n")
	blocks := make([]string, 0, 8)
	runPattern := regexp.MustCompile(`^\s*(?:-\s+)?(?:name:.*\s+)?run:\s*[|>][-+]?(\s*)$`)
	for i := 0; i < len(lines); i++ {
		match := runPattern.FindStringSubmatch(lines[i])
		if match == nil {
			continue
		}
		// run: 所在行的缩进决定 block 的基准缩进。
		baseIndent := len(lines[i]) - len(strings.TrimLeft(lines[i], " \t"))
		var block []string
		for j := i + 1; j < len(lines); j++ {
			line := lines[j]
			if strings.TrimSpace(line) == "" {
				block = append(block, "")
				continue
			}
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			if indent <= baseIndent {
				break
			}
			block = append(block, line)
		}
		blocks = append(blocks, strings.Join(block, "\n"))
	}
	return blocks
}

func sanitizeFileName(name string) string {
	replacer := strings.NewReplacer("/", "-", "#", "--", "[", "(", "]", ")", " ", "_", ":", "-")
	return replacer.Replace(name)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
