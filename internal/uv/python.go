package uv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/mirror"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const (
	maxPythonVersionFileBytes = 128
	maxPyProjectFileBytes     = 1 << 20
)

// PythonVersion 是精确的 CPython major.minor.patch 值对象。
type PythonVersion struct {
	Major int
	Minor int
	Patch int
}

// String 返回 Python 版本的规范形式。
func (v PythonVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// PythonSpec 保存项目声明的精确 Python 与 requires-python 约束。
type PythonSpec struct {
	Version        PythonVersion
	RequiresPython string
}

// PythonRequest 描述一次受管 Python 准备操作。
type PythonRequest struct {
	ProjectDir       string
	PythonInstallDir string
	ProjectEnvDir    string
	CacheDir         string
	Branch           string
	Commit           string
	MirrorPolicy     mirror.Policy
	Reinstall        bool
	Line             LineFunc
}

// PythonResult 保存已验证的 Python 版本。
type PythonResult struct {
	Spec PythonSpec
}

// PythonCheckResult 保存不改变磁盘的 Python 检查结果。
type PythonCheckResult struct {
	Spec PythonSpec
}

// Runner 是 Python 服务消费的最小 uv 执行能力。
type Runner interface {
	Run(ctx context.Context, args []string, options RunOptions) (UVResult, error)
}

// PythonService 负责读取项目 Python 契约并准备受管解释器。
type PythonService struct {
	layout  *config.Layout
	runner  Runner
	network *networkExecutor
}

// NewPythonService 创建 Python 服务。
func NewPythonService(layout *config.Layout, runner Runner) (*PythonService, error) {
	if layout == nil || runner == nil {
		return nil, errors.New("python service dependencies are incomplete")
	}
	network, err := newDefaultNetworkExecutor()
	if err != nil {
		return nil, err
	}
	return &PythonService{layout: layout, runner: runner, network: network}, nil
}

// ReadSpec 只读取项目的版本文件和 pyproject.toml，不启动任何进程。
func (s *PythonService) ReadSpec(ctx context.Context, projectDir string) (PythonSpec, error) {
	if ctx == nil || s == nil || s.layout == nil {
		return PythonSpec{}, errors.New("python specification request is invalid")
	}
	return ReadPythonSpec(ctx, s.layout, projectDir)
}

// ReadPythonSpec 只从受管项目文件读取并校验精确 Python 契约。
func ReadPythonSpec(ctx context.Context, layout *config.Layout, projectDir string) (PythonSpec, error) {
	if ctx == nil || layout == nil {
		return PythonSpec{}, errors.New("python specification request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return PythonSpec{}, err
	}
	if projectDir == "" {
		projectDir = layout.RepoDir()
	}
	versionPath := layout.PythonVersionFile()
	if projectDir != layout.RepoDir() {
		versionPath = filepath.Join(projectDir, ".python-version")
	}
	version, err := readExactPythonVersion(versionPath)
	if err != nil {
		return PythonSpec{}, err
	}
	pyprojectPath := layout.PyProjectFile()
	if projectDir != layout.RepoDir() {
		pyprojectPath = filepath.Join(projectDir, "pyproject.toml")
	}
	requiresPython, err := readRequiresPython(pyprojectPath)
	if err != nil {
		return PythonSpec{}, pythonError(
			protocol.CodePythonVersionIncompatible,
			protocol.StagePythonCheck,
			"项目 Python 约束无法解析",
			map[string]any{},
			err,
		)
	}
	if err := requirementMatches(version, requiresPython); err != nil {
		return PythonSpec{}, pythonError(
			protocol.CodePythonVersionIncompatible,
			protocol.StagePythonCheck,
			"Python 版本与项目约束不兼容",
			map[string]any{"pythonVersion": version.String()},
			err,
		)
	}
	return PythonSpec{Version: version, RequiresPython: requiresPython}, nil
}

// Prepare 检查 uv 的结构化 Python 清单、安装精确版本并复核可发现性。
func (s *PythonService) Prepare(ctx context.Context, request PythonRequest) (PythonResult, error) {
	if ctx == nil || s == nil || s.layout == nil || s.runner == nil {
		return PythonResult{}, errors.New("python preparation request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return PythonResult{}, err
	}
	if request.ProjectDir == "" {
		request.ProjectDir = s.layout.RepoDir()
	}
	if request.PythonInstallDir == "" {
		request.PythonInstallDir = s.layout.PythonDir()
	}
	if request.ProjectEnvDir == "" {
		request.ProjectEnvDir = s.layout.VenvDir()
	}
	if request.CacheDir == "" {
		request.CacheDir = s.layout.UVCacheDir()
	}
	spec, err := s.ReadSpec(ctx, request.ProjectDir)
	if err != nil {
		return PythonResult{}, err
	}
	runOptions := RunOptions{
		Stage:            protocol.StagePythonCheck,
		ProjectDir:       request.ProjectDir,
		PythonInstallDir: request.PythonInstallDir,
		ProjectEnvDir:    request.ProjectEnvDir,
		CacheDir:         request.CacheDir,
		PythonVersion:    spec.Version.String(),
		Branch:           request.Branch,
		Commit:           request.Commit,
		Line:             request.Line,
	}
	if err := s.checkSupported(ctx, runOptions, spec.Version); err != nil {
		return PythonResult{}, err
	}
	target, err := mirror.NewTarget(mirror.TargetSpec{PythonVersion: spec.Version.String()})
	if err != nil {
		return PythonResult{}, fmt.Errorf("build Python mirror target: %w", err)
	}
	installArgs := []string{
		"python",
		"install",
		spec.Version.String(),
		"--managed-python",
		"--install-dir",
		request.PythonInstallDir,
		"--no-bin",
		"--no-registry",
	}
	if request.Reinstall {
		installArgs = append(installArgs, "--reinstall")
	}
	installResult, err := s.network.run(ctx, s.runner, request.MirrorPolicy, mirror.KindPython, target, installArgs, RunOptions{
		Stage:            protocol.StagePythonInstall,
		ProjectDir:       request.ProjectDir,
		PythonInstallDir: request.PythonInstallDir,
		ProjectEnvDir:    request.ProjectEnvDir,
		CacheDir:         request.CacheDir,
		PythonVersion:    spec.Version.String(),
		Branch:           request.Branch,
		Commit:           request.Commit,
		Line:             request.Line,
	})
	if err != nil {
		if isNetworkPolicyError(err) {
			return PythonResult{}, err
		}
		return PythonResult{}, pythonError(
			protocol.CodePythonInstallFailed,
			protocol.StagePythonInstall,
			"Python 安装失败",
			map[string]any{"pythonVersion": spec.Version.String(), "exitCode": installResult.ExitCode},
			err,
		)
	}
	findResult, err := s.runner.Run(ctx, []string{
		"python",
		"find",
		spec.Version.String(),
		"--managed-python",
		"--no-python-downloads",
	}, RunOptions{
		Stage:            protocol.StagePythonCheck,
		ProjectDir:       request.ProjectDir,
		PythonInstallDir: request.PythonInstallDir,
		ProjectEnvDir:    request.ProjectEnvDir,
		CacheDir:         request.CacheDir,
		PythonVersion:    spec.Version.String(),
		Branch:           request.Branch,
		Commit:           request.Commit,
		Line:             request.Line,
	})
	if err != nil || findResult.ExitCode != 0 {
		return PythonResult{}, pythonError(
			protocol.CodePythonVersionMismatch,
			protocol.StagePythonCheck,
			"受管 Python 版本复核失败",
			map[string]any{"pythonVersion": spec.Version.String(), "exitCode": findResult.ExitCode},
			err,
		)
	}
	return PythonResult{Spec: spec}, nil
}

// Check 只验证项目约束、uv 清单和已安装的受管 Python，不执行安装。
func (s *PythonService) Check(ctx context.Context, request PythonRequest) (PythonCheckResult, error) {
	if ctx == nil || s == nil || s.layout == nil || s.runner == nil {
		return PythonCheckResult{}, errors.New("python check request is invalid")
	}
	if err := ctx.Err(); err != nil {
		return PythonCheckResult{}, err
	}
	if request.ProjectDir == "" {
		request.ProjectDir = s.layout.RepoDir()
	}
	if request.PythonInstallDir == "" {
		request.PythonInstallDir = s.layout.PythonDir()
	}
	if request.ProjectEnvDir == "" {
		request.ProjectEnvDir = s.layout.VenvDir()
	}
	if request.CacheDir == "" {
		request.CacheDir = s.layout.UVCacheDir()
	}
	spec, err := s.ReadSpec(ctx, request.ProjectDir)
	if err != nil {
		return PythonCheckResult{}, err
	}
	options := RunOptions{
		Stage:            protocol.StagePythonCheck,
		ProjectDir:       request.ProjectDir,
		PythonInstallDir: request.PythonInstallDir,
		ProjectEnvDir:    request.ProjectEnvDir,
		CacheDir:         request.CacheDir,
		PythonVersion:    spec.Version.String(),
		Branch:           request.Branch,
		Commit:           request.Commit,
		Line:             request.Line,
	}
	if err := s.checkSupported(ctx, options, spec.Version); err != nil {
		return PythonCheckResult{}, err
	}
	if err := s.checkInstalled(ctx, options, spec.Version); err != nil {
		return PythonCheckResult{}, err
	}
	return PythonCheckResult{Spec: spec}, nil
}

func (s *PythonService) checkSupported(
	ctx context.Context,
	options RunOptions,
	version PythonVersion,
) error {
	options = withOfflineUV(options)
	result, err := s.runner.Run(ctx, []string{
		"python",
		"list",
		"--managed-python",
		"--output-format",
		"json",
	}, options)
	if err != nil || result.ExitCode != 0 {
		return pythonError(
			protocol.CodePythonVersionUnsupported,
			protocol.StagePythonCheck,
			"uv 不支持目标 Python 版本",
			map[string]any{"pythonVersion": version.String(), "exitCode": result.ExitCode},
			err,
		)
	}
	if !jsonContainsPythonVersion(result.Stdout, version.String()) {
		return pythonError(
			protocol.CodePythonVersionUnsupported,
			protocol.StagePythonCheck,
			"uv 不支持目标 Python 版本",
			map[string]any{"pythonVersion": version.String()},
			errors.New("target Python version is absent from uv JSON inventory"),
		)
	}
	return nil
}

func (s *PythonService) checkInstalled(
	ctx context.Context,
	options RunOptions,
	version PythonVersion,
) error {
	options = withOfflineUV(options)
	result, err := s.runner.Run(ctx, []string{
		"python",
		"find",
		version.String(),
		"--managed-python",
		"--no-python-downloads",
	}, options)
	if err != nil || result.ExitCode != 0 {
		return pythonError(
			protocol.CodePythonVersionMismatch,
			protocol.StagePythonCheck,
			"受管 Python 版本复核失败",
			map[string]any{"pythonVersion": version.String(), "exitCode": result.ExitCode},
			err,
		)
	}
	return nil
}

func withOfflineUV(options RunOptions) RunOptions {
	options = cloneRunOptions(options)
	options.Environment[uvOfflineEnv] = "1"
	return options
}

func pythonError(
	code protocol.Code,
	stage protocol.Stage,
	message string,
	details map[string]any,
	cause error,
) error {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	return newError(code, stage, message, details, cause)
}

func readExactPythonVersion(path string) (PythonVersion, error) {
	contents, err := readManagedRegularFile(path, maxPythonVersionFileBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return PythonVersion{}, pythonError(
			protocol.CodePythonVersionFileMissing,
			protocol.StagePythonCheck,
			"缺少 Python 版本文件",
			map[string]any{},
			err,
		)
	}
	if err != nil {
		return PythonVersion{}, pythonError(
			protocol.CodePythonVersionInvalid,
			protocol.StagePythonCheck,
			"Python 版本文件无效",
			map[string]any{},
			err,
		)
	}
	if len(contents) > maxPythonVersionFileBytes {
		return PythonVersion{}, pythonError(
			protocol.CodePythonVersionInvalid,
			protocol.StagePythonCheck,
			"Python 版本文件无效",
			map[string]any{},
			errors.New("Python version file is too large"),
		)
	}
	value := strings.TrimSuffix(string(contents), "\n")
	value = strings.TrimSuffix(value, "\r")
	if strings.ContainsAny(value, "\r\n \t") {
		return PythonVersion{}, pythonError(
			protocol.CodePythonVersionInvalid,
			protocol.StagePythonCheck,
			"Python 版本文件无效",
			map[string]any{},
			errors.New("Python version token contains whitespace"),
		)
	}
	version, err := parseExactPythonVersion(value)
	if err != nil {
		return PythonVersion{}, pythonError(
			protocol.CodePythonVersionInvalid,
			protocol.StagePythonCheck,
			"Python 版本文件无效",
			map[string]any{},
			err,
		)
	}
	return version, nil
}

func parseExactPythonVersion(value string) (PythonVersion, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return PythonVersion{}, errors.New("Python version must contain major minor and patch")
	}
	values := [3]int{}
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return PythonVersion{}, errors.New("Python version component is invalid")
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return PythonVersion{}, errors.New("Python version contains a non-numeric component")
			}
		}
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed > 9999 {
			return PythonVersion{}, errors.New("Python version component is out of range")
		}
		values[index] = parsed
	}
	return PythonVersion{Major: values[0], Minor: values[1], Patch: values[2]}, nil
}

func readRequiresPython(path string) (string, error) {
	contents, err := readManagedRegularFile(path, maxPyProjectFileBytes)
	if err != nil {
		return "", err
	}
	return parseProjectRequiresPythonTOML(string(contents))
}

func parseProjectRequiresPythonTOML(document string) (string, error) {
	lines := strings.Split(strings.ReplaceAll(document, "\r\n", "\n"), "\n")
	table := []string(nil)
	arrayTable := false
	found := false
	var requiresPython string
	for index := 0; index < len(lines); index++ {
		line := strings.TrimSpace(tomlTrimCommentLine(lines[index]))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[[") {
			parsed, err := parseTomlArrayTableHeader(line)
			if err != nil {
				return "", err
			}
			table = parsed
			arrayTable = true
			continue
		}
		if strings.HasPrefix(line, "[") {
			parsed, err := parseTomlTableHeader(line)
			if err != nil {
				return "", err
			}
			table = parsed
			arrayTable = false
			continue
		}
		keyText, valueText, ok := splitTomlAssignment(line)
		if !ok {
			continue
		}
		keys, err := parseTomlKeyPath(keyText)
		if err != nil {
			return "", err
		}
		isTarget := !arrayTable && ((len(table) == 1 && table[0] == "project" &&
			len(keys) == 1 && keys[0] == "requires-python") ||
			(len(table) == 0 && len(keys) == 2 && keys[0] == "project" && keys[1] == "requires-python"))
		trimmedValue := strings.TrimSpace(valueText)
		if strings.HasPrefix(trimmedValue, "\"\"\"") || strings.HasPrefix(trimmedValue, "'''") {
			value, consumed, parseErr := parseTomlMultilineString(trimmedValue, lines[index+1:])
			if parseErr != nil {
				return "", parseErr
			}
			index += consumed
			if isTarget {
				if found {
					return "", errors.New("requires-python is duplicated")
				}
				found = true
				requiresPython = value
			}
			continue
		}
		if !isTarget {
			continue
		}
		if found {
			return "", errors.New("requires-python is duplicated")
		}
		value, parseErr := parseTomlStringLiteral(trimmedValue)
		if parseErr != nil {
			return "", parseErr
		}
		found = true
		requiresPython = value
	}
	if !found {
		return "", errors.New("project requires-python is missing")
	}
	return requiresPython, nil
}

func parseTomlTableHeader(line string) ([]string, error) {
	if !strings.HasPrefix(line, "[") || strings.HasPrefix(line, "[[") {
		return nil, errors.New("TOML table header is invalid")
	}
	quote := byte(0)
	escaped := false
	end := -1
	for index := 1; index < len(line); index++ {
		character := line[index]
		if quote != 0 {
			if quote == '"' && escaped {
				escaped = false
				continue
			}
			if quote == '"' && character == '\\' {
				escaped = true
				continue
			}
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if character == ']' {
			end = index
			break
		}
	}
	if end < 0 || strings.TrimSpace(tomlTrimCommentLine(line[end+1:])) != "" {
		return nil, errors.New("TOML table header is invalid")
	}
	return parseTomlKeyPath(line[1:end])
}

func parseTomlArrayTableHeader(line string) ([]string, error) {
	if !strings.HasPrefix(line, "[[") {
		return nil, errors.New("TOML array table header is invalid")
	}
	quote := byte(0)
	escaped := false
	end := -1
	for index := 2; index+1 < len(line); index++ {
		character := line[index]
		if quote != 0 {
			if quote == '"' && escaped {
				escaped = false
				continue
			}
			if quote == '"' && character == '\\' {
				escaped = true
				continue
			}
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if character == ']' && line[index+1] == ']' {
			end = index
			break
		}
	}
	if end < 0 || strings.TrimSpace(tomlTrimCommentLine(line[end+2:])) != "" {
		return nil, errors.New("TOML array table header is invalid")
	}
	return parseTomlKeyPath(line[2:end])
}

func parseTomlKeyPath(value string) ([]string, error) {
	parts := make([]string, 0, 2)
	start := 0
	quote := byte(0)
	escaped := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if quote != 0 {
			if quote == '"' && escaped {
				escaped = false
				continue
			}
			if quote == '"' && character == '\\' {
				escaped = true
				continue
			}
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if character == '.' {
			part, err := parseTomlKey(strings.TrimSpace(value[start:index]))
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
			start = index + 1
		}
	}
	if quote != 0 {
		return nil, errors.New("TOML key is unterminated")
	}
	part, err := parseTomlKey(strings.TrimSpace(value[start:]))
	if err != nil {
		return nil, err
	}
	return append(parts, part), nil
}

func parseTomlKey(value string) (string, error) {
	if value == "" {
		return "", errors.New("TOML key is empty")
	}
	if value[0] == '\'' || value[0] == '"' {
		return parseTomlStringLiteral(value)
	}
	for _, character := range value {
		if !(character == '_' || character == '-' || character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' || character >= '0' && character <= '9') {
			return "", errors.New("TOML bare key is invalid")
		}
	}
	return value, nil
}

func splitTomlAssignment(line string) (string, string, bool) {
	quote := byte(0)
	escaped := false
	depth := 0
	for index := 0; index < len(line); index++ {
		character := line[index]
		if quote != 0 {
			if quote == '"' && escaped {
				escaped = false
				continue
			}
			if quote == '"' && character == '\\' {
				escaped = true
				continue
			}
			if character == quote {
				quote = 0
			}
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
		case '[', '{':
			depth++
		case ']', '}':
			if depth > 0 {
				depth--
			}
		case '#':
			return "", "", false
		case '=':
			if depth == 0 {
				return strings.TrimSpace(line[:index]), strings.TrimSpace(line[index+1:]), true
			}
		}
	}
	return "", "", false
}

func parseTomlStringLiteral(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 2 || (value[0] != '\'' && value[0] != '"') {
		return "", errors.New("requires-python must be a TOML string")
	}
	quote := value[0]
	end := tomlClosingDelimiter(value, 1, quote)
	if end < 0 || strings.TrimSpace(tomlTrimCommentLine(value[end+1:])) != "" {
		return "", errors.New("requires-python TOML string is invalid")
	}
	inner := value[1:end]
	if quote == '"' {
		decoded, err := decodeTomlBasicString(inner, false)
		if err != nil {
			return "", errors.New("requires-python TOML string is invalid")
		}
		inner = decoded
	}
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return "", errors.New("requires-python TOML string is invalid")
	}
	return inner, nil
}

func parseTomlMultilineString(first string, following []string) (string, int, error) {
	quote := byte('"')
	if strings.HasPrefix(first, "'''") {
		quote = '\''
	}
	open := string([]byte{quote, quote, quote})
	if !strings.HasPrefix(first, open) {
		return "", 0, errors.New("TOML multiline string is invalid")
	}
	parts := make([]string, 0, len(following)+1)
	current := first[len(open):]
	consumed := 0
	for {
		end := tomlClosingTriple(current, quote)
		if end >= 0 {
			tail := strings.TrimSpace(tomlTrimCommentLine(current[end+3:]))
			if tail != "" {
				return "", 0, errors.New("requires-python TOML string is invalid")
			}
			parts = append(parts, current[:end])
			value := strings.Join(parts, "")
			value = strings.TrimPrefix(value, "\n")
			value = strings.TrimPrefix(value, "\r\n")
			if quote == '"' {
				decoded, err := decodeTomlBasicString(value, true)
				if err != nil {
					return "", 0, errors.New("requires-python TOML string is invalid")
				}
				value = decoded
			}
			value = strings.TrimSpace(value)
			if value == "" {
				return "", 0, errors.New("requires-python TOML string is invalid")
			}
			return value, consumed, nil
		}
		parts = append(parts, current, "\n")
		if consumed >= len(following) {
			return "", 0, errors.New("requires-python TOML string is unterminated")
		}
		current = following[consumed]
		consumed++
	}
}

func tomlClosingDelimiter(value string, start int, quote byte) int {
	escaped := false
	for index := start; index < len(value); index++ {
		character := value[index]
		if quote == '"' && escaped {
			escaped = false
			continue
		}
		if quote == '"' && character == '\\' {
			escaped = true
			continue
		}
		if character == quote {
			return index
		}
	}
	return -1
}

func tomlClosingTriple(value string, quote byte) int {
	escaped := false
	for index := 0; index+2 < len(value); index++ {
		character := value[index]
		if quote == '"' && escaped {
			escaped = false
			continue
		}
		if quote == '"' && character == '\\' {
			escaped = true
			continue
		}
		if value[index] == quote && value[index+1] == quote && value[index+2] == quote {
			return index
		}
	}
	return -1
}

func tomlTrimCommentLine(line string) string {
	quote := byte(0)
	triple := false
	escaped := false
	for index := 0; index < len(line); index++ {
		character := line[index]
		if quote != 0 {
			if quote == '"' && escaped {
				escaped = false
				continue
			}
			if quote == '"' && character == '\\' {
				escaped = true
				continue
			}
			if triple {
				if index+2 < len(line) && line[index] == quote && line[index+1] == quote && line[index+2] == quote {
					quote = 0
					triple = false
					index += 2
				}
				continue
			}
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '#' {
			return line[:index]
		}
		if character == '\'' || character == '"' {
			quote = character
			triple = index+2 < len(line) && line[index+1] == character && line[index+2] == character
			if triple {
				index += 2
			}
		}
	}
	return line
}

func decodeTomlBasicString(value string, multiline bool) (string, error) {
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			builder.WriteByte(value[index])
			continue
		}
		if index+1 >= len(value) {
			return "", errors.New("TOML basic string escape is incomplete")
		}
		index++
		switch value[index] {
		case 'b':
			builder.WriteByte('\b')
		case 't':
			builder.WriteByte('\t')
		case 'n':
			builder.WriteByte('\n')
		case 'f':
			builder.WriteByte('\f')
		case 'r':
			builder.WriteByte('\r')
		case '"', '\\':
			builder.WriteByte(value[index])
		case '\r', '\n':
			if !multiline {
				return "", errors.New("TOML basic string contains a newline escape")
			}
			if value[index] == '\r' && index+1 < len(value) && value[index+1] == '\n' {
				index++
			}
			for index+1 < len(value) && (value[index+1] == ' ' || value[index+1] == '\t' || value[index+1] == '\r' || value[index+1] == '\n') {
				index++
			}
		case 'u', 'U':
			digits := 4
			if value[index] == 'U' {
				digits = 8
			}
			if index+digits >= len(value) {
				return "", errors.New("TOML Unicode escape is incomplete")
			}
			code, err := strconv.ParseUint(value[index+1:index+1+digits], 16, 32)
			if err != nil || code > utf8.MaxRune || code >= 0xD800 && code <= 0xDFFF {
				return "", errors.New("TOML Unicode escape is invalid")
			}
			builder.WriteRune(rune(code))
			index += digits
		default:
			return "", errors.New("TOML basic string escape is invalid")
		}
	}
	return builder.String(), nil
}

func readManagedRegularFile(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("managed file is not a regular file")
	}
	if info.Size() > maxBytes {
		return nil, errors.New("managed file is too large")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	if int64(len(contents)) > maxBytes {
		return nil, errors.New("managed file is too large")
	}
	return contents, nil
}

type versionSpecifier struct {
	operator string
	version  [3]int
	parts    int
	wildcard bool
}

func requirementMatches(version PythonVersion, raw string) error {
	parts := strings.Split(raw, ",")
	if len(parts) == 0 {
		return errors.New("requires-python is empty")
	}
	for _, part := range parts {
		specifier, err := parseSpecifier(strings.TrimSpace(part))
		if err != nil || !specifierMatches(version, specifier) {
			if err == nil {
				err = errors.New("Python version does not satisfy requires-python")
			}
			return err
		}
	}
	return nil
}

func parseSpecifier(value string) (versionSpecifier, error) {
	if value == "" {
		return versionSpecifier{}, errors.New("requires-python contains an empty specifier")
	}
	operator := "=="
	for _, candidate := range []string{"~=", ">=", "<=", "!=", ">", "<", "=="} {
		if strings.HasPrefix(value, candidate) {
			operator = candidate
			value = strings.TrimSpace(strings.TrimPrefix(value, candidate))
			break
		}
	}
	wildcard := strings.HasSuffix(value, ".*")
	if wildcard {
		value = strings.TrimSuffix(value, ".*")
	}
	versionParts := strings.Split(value, ".")
	if len(versionParts) < 1 || len(versionParts) > 3 ||
		(wildcard && operator != "==" && operator != "!=") ||
		(operator == "~=" && len(versionParts) < 2) {
		return versionSpecifier{}, errors.New("requires-python version specifier is invalid")
	}
	var parsed [3]int
	for index, part := range versionParts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return versionSpecifier{}, errors.New("requires-python version component is invalid")
		}
		for _, character := range part {
			if character < '0' || character > '9' {
				return versionSpecifier{}, errors.New("requires-python version component is invalid")
			}
		}
		parsedValue, err := strconv.Atoi(part)
		if err != nil || parsedValue > 9999 {
			return versionSpecifier{}, errors.New("requires-python version component is out of range")
		}
		parsed[index] = parsedValue
	}
	return versionSpecifier{operator: operator, version: parsed, parts: len(versionParts), wildcard: wildcard}, nil
}

func specifierMatches(version PythonVersion, specifier versionSpecifier) bool {
	value := [3]int{version.Major, version.Minor, version.Patch}
	if specifier.operator == "==" && specifier.wildcard {
		return value[0] == specifier.version[0] &&
			(specifier.parts < 2 || value[1] == specifier.version[1])
	}
	if specifier.operator == "!=" && specifier.wildcard {
		return !specifierMatches(version, versionSpecifier{
			operator: "==",
			version:  specifier.version,
			parts:    specifier.parts,
			wildcard: true,
		})
	}
	comparison := compareVersion(value, specifier.version)
	switch specifier.operator {
	case "==":
		return comparison == 0
	case "!=":
		return comparison != 0
	case ">":
		return comparison > 0
	case ">=":
		return comparison >= 0
	case "<":
		return comparison < 0
	case "<=":
		return comparison <= 0
	case "~=":
		upper := [3]int{specifier.version[0] + 1, 0, 0}
		if specifier.parts == 3 {
			upper = [3]int{specifier.version[0], specifier.version[1] + 1, 0}
		}
		return comparison >= 0 && compareVersion(value, upper) < 0
	default:
		return false
	}
}

func compareVersion(left, right [3]int) int {
	for index := range left {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}

type pythonListEntry struct {
	Version string `json:"version"`
}

func jsonContainsPythonVersion(raw, expected string) bool {
	var entries []pythonListEntry
	if json.Unmarshal([]byte(raw), &entries) == nil {
		for _, entry := range entries {
			if entry.Version == expected {
				return true
			}
		}
		return false
	}
	var envelope struct {
		Installations []pythonListEntry `json:"installations"`
		Versions      []pythonListEntry `json:"versions"`
	}
	if json.Unmarshal([]byte(raw), &envelope) != nil {
		return false
	}
	for _, entry := range append(envelope.Installations, envelope.Versions...) {
		if entry.Version == expected {
			return true
		}
	}
	return false
}
