package uv

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const (
	uvPythonInstallDirEnv = "UV_PYTHON_INSTALL_DIR"
	uvCacheDirEnv         = "UV_CACHE_DIR"
	uvProjectEnvironment  = "UV_PROJECT_ENVIRONMENT"
	uvManagedPythonEnv    = "UV_MANAGED_PYTHON"
	uvNoModifyPathEnv     = "UV_NO_MODIFY_PATH"
	uvPythonInstallBinEnv = "UV_PYTHON_INSTALL_BIN"
	uvColorEnv            = "UV_COLOR"
	uvNoProgressEnv       = "UV_NO_PROGRESS"
	uvNoSystemConfigEnv   = "UV_NO_SYSTEM_CONFIG"
	autoMASUVExecutable   = "AUTO_MAS_UV_EXE"
	autoMASProtocol       = "AUTO_MAS_RUNTIME_PROTOCOL"
	autoMASVersion        = "AUTO_MAS_EXPECTED_VERSION"
	autoMASCommit         = "AUTO_MAS_EXPECTED_COMMIT"
	autoMASSupervised     = "AUTO_MAS_SUPERVISED"
	maxUVOutputLineBytes  = 1 << 20
	maxUVOutputBytes      = 4 << 20
	maxUVStreamBytes      = 16 << 20
)

// RunnerConfig 描述一个受管 uv 执行器的固定目录上下文。
type RunnerConfig struct {
	Executable       string
	ProjectDir       string
	PythonInstallDir string
	ProjectEnvDir    string
	CacheDir         string
	Clock            func() time.Time
}

// RunOptions 描述单次 uv 调用的可变信息。
type RunOptions struct {
	Stage            protocol.Stage
	ProjectDir       string
	PythonInstallDir string
	ProjectEnvDir    string
	CacheDir         string
	// Environment 只接受非受控诊断变量，以及 network.go 明确允许的 UV 网络键；
	// StartManaged 会清除并覆盖全部 AUTO_MAS 监督键。
	Environment   map[string]string
	PythonVersion string
	Branch        string
	Commit        string
	Line          LineFunc
}

// LineFunc 接收 uv stdout/stderr 的逐行诊断。
// Runner 同步调用回调，因此实现必须快速返回、尊重 ctx 取消，不能等待
// 不受控的外部事件；回调返回错误会立即终止 uv 进程。
type LineFunc func(ctx context.Context, stream, line string) error

// UVResult 保存一次 uv 进程的稳定结果与受限诊断摘要。
type UVResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// UVRunner 是 Runtime 唯一的 uv 子进程执行入口。
type UVRunner struct {
	Executable       string
	ProjectDir       string
	PythonInstallDir string
	ProjectEnvDir    string
	CacheDir         string
	Clock            func() time.Time
}

// NewRunner 创建固定可执行文件的 uv 执行器。
func NewRunner(config RunnerConfig) (*UVRunner, error) {
	if config.Executable == "" || config.PythonInstallDir == "" ||
		config.ProjectEnvDir == "" || config.CacheDir == "" {
		return nil, errors.New("uv runner configuration is incomplete")
	}
	if !process.Supported() {
		return nil, process.ErrUnsupported
	}
	return &UVRunner{
		Executable:       config.Executable,
		ProjectDir:       config.ProjectDir,
		PythonInstallDir: config.PythonInstallDir,
		ProjectEnvDir:    config.ProjectEnvDir,
		CacheDir:         config.CacheDir,
		Clock:            config.Clock,
	}, nil
}

// Run 使用参数数组执行 uv，并在退出码非零时返回结构化错误。
func (r *UVRunner) Run(
	ctx context.Context,
	args []string,
	options RunOptions,
) (UVResult, error) {
	if ctx == nil {
		return UVResult{}, newError(
			protocol.CodeUVExecFailed,
			options.Stage,
			"uv 执行失败",
			map[string]any{},
			errors.New("uv runner context is nil"),
		)
	}
	if r == nil || r.Executable == "" || len(args) == 0 {
		return UVResult{}, newError(
			protocol.CodeUVExecFailed,
			options.Stage,
			"uv 执行失败",
			map[string]any{},
			errors.New("uv runner request is invalid"),
		)
	}
	if err := ctx.Err(); err != nil {
		return UVResult{}, runnerCancellationError(options.Stage, map[string]any{}, err)
	}
	resolved := r.resolveOptions(options)
	if err := validateRunnerPaths(resolved); err != nil {
		return UVResult{}, newError(
			protocol.CodeUVExecFailed,
			options.Stage,
			"uv 执行失败",
			map[string]any{},
			err,
		)
	}
	runContext, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	command := exec.CommandContext(runContext, r.Executable, args...)
	command.Dir = resolved.ProjectDir
	command.Env = buildEnvironment(resolved)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return UVResult{}, newError(
			protocol.CodeUVExecFailed,
			options.Stage,
			"uv 执行失败",
			map[string]any{},
			err,
		)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return UVResult{}, newError(
			protocol.CodeUVExecFailed,
			options.Stage,
			"uv 执行失败",
			map[string]any{},
			err,
		)
	}
	clock := r.Clock
	if clock == nil {
		clock = time.Now
	}
	started := clock()
	if err := command.Start(); err != nil {
		return UVResult{}, newError(
			protocol.CodeUVExecFailed,
			options.Stage,
			"uv 执行失败",
			map[string]any{},
			err,
		)
	}
	job, jobErr := process.NewJob()
	if errors.Is(jobErr, process.ErrUnsupported) {
		_ = command.Process.Kill()
		_ = command.Wait()
		return UVResult{}, newError(
			protocol.CodeUnsupportedMode,
			options.Stage,
			"当前平台尚不支持 uv 进程树监督",
			map[string]any{},
			jobErr,
		)
	}
	if jobErr != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return UVResult{}, newError(
			protocol.CodeUVExecFailed,
			options.Stage,
			"uv 进程隔离失败",
			map[string]any{},
			jobErr,
		)
	}
	if job != nil {
		if err := job.Assign(uint32(command.Process.Pid)); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			_ = job.Close()
			return UVResult{}, newError(
				protocol.CodeUVExecFailed,
				options.Stage,
				"uv 进程隔离失败",
				map[string]any{},
				err,
			)
		}
	}
	jobStop := make(chan struct{})
	jobDone := make(chan struct{})
	if job != nil {
		go func() {
			defer close(jobDone)
			select {
			case <-runContext.Done():
				_ = job.Terminate(1)
			case <-jobStop:
			}
		}()
	}
	callbackCtx, cancelCallback := context.WithCancel(runContext)
	defer cancelCallback()
	stdoutResult := make(chan streamResult, 1)
	stderrResult := make(chan streamResult, 1)
	lineSink := func(stream, line string) error {
		if resolved.Line == nil {
			return nil
		}
		if err := callbackCtx.Err(); err != nil {
			return err
		}
		if err := resolved.Line(callbackCtx, stream, line); err != nil {
			cancelCallback()
			cancelRun()
			if job != nil {
				_ = job.Terminate(1)
			}
			return err
		}
		return nil
	}
	outputOverflow := func() {
		cancelCallback()
		cancelRun()
		if job != nil {
			_ = job.Terminate(1)
		}
	}
	go readUVStream("stdout", stdout, lineSink, outputOverflow, stdoutResult)
	go readUVStream("stderr", stderr, lineSink, outputOverflow, stderrResult)
	waitErr := command.Wait()
	if job != nil {
		close(jobStop)
		<-jobDone
		if err := job.Close(); err != nil {
			waitErr = errors.Join(waitErr, err)
		}
	}
	stdoutValue := <-stdoutResult
	stderrValue := <-stderrResult
	duration := clock().Sub(started)
	result := UVResult{
		ExitCode: exitCode(waitErr),
		Stdout:   stdoutValue.output,
		Stderr:   stderrValue.output,
		Duration: duration,
	}
	streamErr := errors.Join(stdoutValue.err, stderrValue.err)
	if err := ctx.Err(); err != nil {
		return result, runnerCancellationError(options.Stage, runnerDetails(result, resolved), err)
	}
	if streamErr != nil {
		return result, newError(
			protocol.CodeUVExecFailed,
			options.Stage,
			"uv 输出读取失败",
			runnerDetails(result, resolved),
			streamErr,
		)
	}
	if waitErr != nil {
		return result, newError(
			protocol.CodeUVExecFailed,
			options.Stage,
			"uv 执行失败",
			mergeDetails(runnerDetails(result, resolved), map[string]any{
				"exitCode": result.ExitCode,
			}),
			waitErr,
		)
	}
	return result, nil
}

// CheckVersion 验证受管 uv 的 --version 输出是否报告固定版本。
func (r *UVRunner) CheckVersion(
	ctx context.Context,
	expected string,
	stage protocol.Stage,
	line LineFunc,
) error {
	result, err := r.Run(ctx, []string{"--version"}, RunOptions{Stage: stage, Line: line})
	if err != nil {
		return err
	}
	if !uvVersionOutputMatches(result.Stdout, expected) {
		return newError(
			protocol.CodeUVVersionMismatch,
			stage,
			"uv 版本校验失败",
			map[string]any{
				"expectedVersion": expected,
			},
			errors.New("uv reported an unexpected version"),
		)
	}
	return nil
}

func uvVersionOutputMatches(output, expected string) bool {
	if expected == "" || strings.ContainsAny(expected, " \t\r\n") {
		return false
	}
	normalized := normalizeVersionOutput(output)
	if strings.ContainsAny(normalized, "\t\r\n") || !strings.HasPrefix(normalized, "uv ") {
		return false
	}
	reported, metadata, hasMetadata := strings.Cut(strings.TrimPrefix(normalized, "uv "), " ")
	if reported != expected {
		return false
	}
	if !hasMetadata {
		return true
	}
	if len(metadata) < 3 || metadata[0] != '(' || metadata[len(metadata)-1] != ')' {
		return false
	}
	inner := metadata[1 : len(metadata)-1]
	return inner != "" && strings.TrimSpace(inner) == inner && !strings.ContainsAny(inner, "()\r\n")
}

func normalizeVersionOutput(output string) string {
	if strings.HasSuffix(output, "\r\n") {
		return strings.TrimSuffix(output, "\r\n")
	}
	if strings.HasSuffix(output, "\n") {
		return strings.TrimSuffix(output, "\n")
	}
	return output
}

type resolvedRunOptions struct {
	ProjectDir       string
	PythonInstallDir string
	ProjectEnvDir    string
	CacheDir         string
	Environment      map[string]string
	Line             LineFunc
	PythonVersion    string
	Branch           string
	Commit           string
}

func (r *UVRunner) resolveOptions(options RunOptions) resolvedRunOptions {
	values := resolvedRunOptions{
		ProjectDir:       r.ProjectDir,
		PythonInstallDir: r.PythonInstallDir,
		ProjectEnvDir:    r.ProjectEnvDir,
		CacheDir:         r.CacheDir,
		Environment:      cloneEnvironment(options.Environment),
		Line:             options.Line,
		PythonVersion:    options.PythonVersion,
		Branch:           options.Branch,
		Commit:           options.Commit,
	}
	if options.ProjectDir != "" {
		values.ProjectDir = options.ProjectDir
	}
	if options.PythonInstallDir != "" {
		values.PythonInstallDir = options.PythonInstallDir
	}
	if options.ProjectEnvDir != "" {
		values.ProjectEnvDir = options.ProjectEnvDir
	}
	if options.CacheDir != "" {
		values.CacheDir = options.CacheDir
	}
	return values
}

func validateRunnerPaths(options resolvedRunOptions) error {
	for name, path := range map[string]string{
		"project directory":             options.ProjectDir,
		"python install directory":      options.PythonInstallDir,
		"project environment directory": options.ProjectEnvDir,
		"uv cache directory":            options.CacheDir,
	} {
		if path == "" || strings.ContainsRune(path, '\x00') {
			return errors.New(name + " is invalid")
		}
	}
	return nil
}

func buildEnvironment(options resolvedRunOptions) []string {
	return buildEnvironmentWithSupervision(options, nil)
}

func buildEnvironmentWithSupervision(options resolvedRunOptions, supervision map[string]string) []string {
	controlled := map[string]string{
		uvPythonInstallDirEnv: options.PythonInstallDir,
		uvCacheDirEnv:         options.CacheDir,
		uvProjectEnvironment:  options.ProjectEnvDir,
		uvManagedPythonEnv:    "1",
		uvNoModifyPathEnv:     "1",
		uvPythonInstallBinEnv: "0",
		uvColorEnv:            "never",
		uvNoProgressEnv:       "1",
		uvNoSystemConfigEnv:   "1",
	}
	for key, value := range supervision {
		controlled[key] = value
	}
	reserved := make([]string, 0, len(controlled)+1)
	for key := range controlled {
		reserved = append(reserved, key)
	}
	overrides := make(map[string]string, len(options.Environment))
	optionKeys := make([]string, 0, len(options.Environment))
	for key := range options.Environment {
		optionKeys = append(optionKeys, key)
	}
	sort.Strings(optionKeys)
	for _, key := range optionKeys {
		value := options.Environment[key]
		canonicalKey, allowedUV := canonicalUVOverrideKey(key)
		_, supervisionKey := canonicalSupervisionEnvironmentKey(key)
		if strings.EqualFold(key, "PATH") || containsEnvironmentKey(reserved, key) ||
			(isUVEnvironmentKey(key) && !allowedUV) || supervisionKey {
			continue
		}
		if allowedUV {
			key = canonicalKey
		}
		overrides[key] = value
	}
	values := make([]string, 0, len(os.Environ())+len(controlled)+len(overrides))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if isUVEnvironmentKey(key) || isSupervisionEnvironmentKey(key) || containsEnvironmentKey(reserved, key) ||
				containsEnvironmentKeyMap(overrides, key) {
				continue
			}
		}
		values = append(values, entry)
	}
	keys := make([]string, 0, len(controlled)+len(overrides))
	for key := range controlled {
		keys = append(keys, key)
	}
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, managed := controlled[key]
		if !managed {
			value = overrides[key]
		}
		values = append(values, key+"="+value)
	}
	return values
}

func canonicalSupervisionEnvironmentKey(key string) (string, bool) {
	for _, managed := range []string{
		autoMASUVExecutable,
		autoMASProtocol,
		autoMASVersion,
		autoMASCommit,
		autoMASSupervised,
	} {
		if strings.EqualFold(key, managed) {
			return managed, true
		}
	}
	return "", false
}

func isSupervisionEnvironmentKey(key string) bool {
	_, ok := canonicalSupervisionEnvironmentKey(key)
	return ok
}

func isUVEnvironmentKey(key string) bool {
	return len(key) >= 3 && strings.EqualFold(key[:3], "UV_")
}

func canonicalUVOverrideKey(key string) (string, bool) {
	switch {
	case strings.EqualFold(key, uvOfflineEnv):
		return uvOfflineEnv, true
	case strings.EqualFold(key, uvPythonInstallMirrorEnv):
		return uvPythonInstallMirrorEnv, true
	default:
		return "", false
	}
}

func containsEnvironmentKey(keys []string, want string) bool {
	for _, key := range keys {
		if strings.EqualFold(key, want) {
			return true
		}
	}
	return false
}

func containsEnvironmentKeyMap(values map[string]string, want string) bool {
	for key := range values {
		if strings.EqualFold(key, want) {
			return true
		}
	}
	return false
}

type streamResult struct {
	output string
	err    error
}

func readUVStream(
	stream string,
	reader io.Reader,
	callback func(string, string) error,
	overflow func(),
	result chan<- streamResult,
) {
	var builder strings.Builder
	readerWithBuffer := bufio.NewReader(reader)
	var line []byte
	lineTooLong := false
	var streamErr error
	var consumed int64
	overflowed := false
	processLine := func(value []byte, hasNewline bool) {
		if len(value) == 0 && !hasNewline {
			return
		}
		if builder.Len() < maxUVOutputBytes {
			remaining := maxUVOutputBytes - builder.Len()
			if len(value) > remaining {
				_, _ = builder.Write(value[:remaining])
				recordFirstError(&streamErr, errors.New("uv output exceeds capture limit"))
			} else {
				_, _ = builder.Write(value)
			}
		} else {
			recordFirstError(&streamErr, errors.New("uv output exceeds capture limit"))
		}
		if consumed > maxUVStreamBytes && !overflowed {
			overflowed = true
			recordFirstError(&streamErr, errors.New("uv output exceeds stream limit"))
			if overflow != nil {
				overflow()
			}
		}
		if callback != nil {
			lineValue := strings.TrimSuffix(string(value), "\n")
			lineValue = strings.TrimSuffix(lineValue, "\r")
			callbackErr := callback(stream, lineValue)
			recordFirstError(&streamErr, callbackErr)
		}
	}
	for {
		fragment, err := readerWithBuffer.ReadSlice('\n')
		if len(fragment) > 0 {
			consumed += int64(len(fragment))
			if consumed > maxUVStreamBytes && !overflowed {
				overflowed = true
				recordFirstError(&streamErr, errors.New("uv output exceeds stream limit"))
				if overflow != nil {
					overflow()
				}
			}
			if !lineTooLong {
				if len(line)+len(fragment) > maxUVOutputLineBytes {
					lineTooLong = true
					line = nil
					recordFirstError(&streamErr, errors.New("uv output line exceeds size limit"))
					if overflow != nil {
						overflow()
					}
				} else {
					line = append(line, fragment...)
				}
			}
			if fragment[len(fragment)-1] == '\n' {
				if !lineTooLong {
					processLine(line, true)
				}
				line = nil
				lineTooLong = false
			}
		}
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if errors.Is(err, io.EOF) {
				if !lineTooLong && len(line) > 0 {
					processLine(line, false)
				}
				break
			}
			recordFirstError(&streamErr, err)
			break
		}
	}
	result <- streamResult{output: builder.String(), err: streamErr}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func runnerDetails(result UVResult, options resolvedRunOptions) map[string]any {
	return map[string]any{
		"exitCode":      result.ExitCode,
		"durationMs":    result.Duration.Milliseconds(),
		"pythonVersion": options.PythonVersion,
		"branch":        options.Branch,
		"commit":        options.Commit,
	}
}

func mergeDetails(left, right map[string]any) map[string]any {
	merged := cloneDetails(left)
	for key, value := range right {
		merged[key] = value
	}
	return merged
}

func cloneEnvironment(environment map[string]string) map[string]string {
	if len(environment) == 0 {
		return map[string]string{}
	}
	cloned := make(map[string]string, len(environment))
	for key, value := range environment {
		cloned[key] = value
	}
	return cloned
}

func runnerCancellationError(stage protocol.Stage, details map[string]any, cause error) error {
	return newError(
		protocol.CodeOperationCancelled,
		stage,
		"操作已取消",
		details,
		cause,
	)
}

func recordFirstError(target *error, cause error) {
	if target != nil && *target == nil && cause != nil {
		*target = cause
	}
}

// EnvironmentForTesting 返回受控环境的稳定键值视图，供假 uv 夹具断言。
func (r *UVRunner) EnvironmentForTesting(options RunOptions) map[string]string {
	values := buildEnvironment(r.resolveOptions(options))
	result := make(map[string]string, len(values))
	for _, value := range values {
		key, raw, found := strings.Cut(value, "=")
		if found {
			result[key] = raw
		}
	}
	return result
}

// CommandEnvironmentDetails 返回不会包含外部输出的调试字段。
func (r *UVRunner) CommandEnvironmentDetails(options RunOptions) map[string]any {
	resolved := r.resolveOptions(options)
	return map[string]any{
		"projectDir":       filepath.Clean(resolved.ProjectDir),
		"pythonInstallDir": filepath.Clean(resolved.PythonInstallDir),
		"projectEnvDir":    filepath.Clean(resolved.ProjectEnvDir),
		"cacheDir":         filepath.Clean(resolved.CacheDir),
		"pythonVersion":    resolved.PythonVersion,
		"branch":           resolved.Branch,
		"commit":           resolved.Commit,
	}
}
