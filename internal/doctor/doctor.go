package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/lock"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// maxStateFileCheckBytes 是 runtime-state 检查的原始文件大小上限。
const maxStateFileCheckBytes = 1 << 20

// maxVersionFileBytes 是 repo/res/version.json 的大小上限。
const maxVersionFileBytes = 1 << 20

// errStateFileTooLarge 表示状态文件超出只读检查的大小上限。
var errStateFileTooLarge = errors.New("state file exceeds size limit")

// errMalformedPayload 表示文件存在但内容不可解析，details 稳定分类为 invalid。
var errMalformedPayload = errors.New("malformed payload")

// Status 是单个检查项的稳定结果。
type Status string

const (
	StatusOK      Status = "ok"
	StatusMissing Status = "missing"
	StatusError   Status = "error"
)

// Valid 报告 Status 是否属于稳定全集。
func (s Status) Valid() bool {
	switch s {
	case StatusOK, StatusMissing, StatusError:
		return true
	default:
		return false
	}
}

// String 返回 Status 的稳定字面量。
func (s Status) String() string { return string(s) }

// Check 描述一次检查的名称、稳定结果与补充事实。
type Check struct {
	ID      string
	Name    string
	Status  Status
	Message string
	Details map[string]any
}

// Summary 汇总一次诊断的计数。
type Summary struct {
	Total   int
	OK      int
	Missing int
	Error   int
}

// Worst 返回本次诊断的最差检查项状态，用于汇总输出。
// 空报告视为 ok：没有检查项就没有坏消息。
func (s Summary) Worst() Status {
	switch {
	case s.Error > 0:
		return StatusError
	case s.Missing > 0:
		return StatusMissing
	default:
		return StatusOK
	}
}

// Report 是 doctor 的结构化输出。
type Report struct {
	Checks  []Check
	Summary Summary
}

// Probes 是 doctor 需要注入替身的只读探测点。
type Probes struct {
	UVVersion func(ctx context.Context, exePath string) (string, error)
	DiskFree  func(ctx context.Context, path string) (uint64, error)
}

// Service 组合布局与探针执行全部检查。
type Service struct {
	layout *config.Layout
	probes Probes
}

// New 创建只读诊断服务，layout 与两个探针都不可为空。
func New(layout *config.Layout, probes Probes) (*Service, error) {
	if layout == nil || probes.UVVersion == nil || probes.DiskFree == nil {
		return nil, errors.New("doctor service requires layout and probes")
	}
	return &Service{layout: layout, probes: probes}, nil
}

// Run 执行全部检查并逐项发射 progress 事件。
// 单项异常只影响该项状态，不提前终止；Run 正常路径不失败。
func (s *Service) Run(ctx context.Context, emitter *protocol.Emitter) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	var checks []Check
	for _, run := range []func(context.Context) Check{
		s.checkAppRoot,
		s.checkLayout,
		s.checkUV,
		s.checkPython,
		s.checkRepo,
		s.checkVenv,
		s.checkRuntimeState,
		s.checkMutexes,
		s.checkDisk,
	} {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		check := run(ctx)
		if err := emitCheck(emitter, check); err != nil {
			return Report{}, err
		}
		checks = append(checks, check)
	}
	summary := summarize(checks)
	if err := emitSummary(emitter, summary); err != nil {
		return Report{}, err
	}
	return Report{Checks: checks, Summary: summary}, nil
}

func emitCheck(emitter *protocol.Emitter, check Check) error {
	if err := emitter.EmitProgress(protocol.ProgressEvent{
		Stage:   protocol.StageDoctor,
		Status:  protocol.ProgressRunning,
		Message: check.Name,
	}); err != nil {
		return err
	}
	message := fmt.Sprintf("%s：%s", check.Name, check.Message)
	return emitter.EmitProgress(protocol.ProgressEvent{
		Stage:   protocol.StageDoctor,
		Status:  progressForStatus(check.Status),
		Message: message,
	})
}

// emitSummary 在全部检查项之后发射一条汇总 progress。
// human renderer 不渲染 result.details，若不发这一条，默认输出模式下
// result.details.checks/summary 这张唯一的结构化检查表对用户完全不可见，
// 只能靠逐条中文 message 自行汇总（AGENTS 8.11 禁止依赖的做法）。
func emitSummary(emitter *protocol.Emitter, summary Summary) error {
	return emitter.EmitProgress(protocol.ProgressEvent{
		Stage:  protocol.StageDoctor,
		Status: progressForStatus(summary.Worst()),
		Message: fmt.Sprintf(
			"诊断汇总：共 %d 项，正常 %d 项，缺失 %d 项，异常 %d 项",
			summary.Total, summary.OK, summary.Missing, summary.Error,
		),
	})
}

// progressForStatus 把检查项状态映射为协议 progress 状态。
// missing 必须映射 skipped 而非 succeeded：progress 状态是 human 模式下
// 用户唯一能看到的机器语义，把「未安装受管 uv」渲染成 succeeded 会让一个
// 空 app-root 报出满屏全绿（T3.7 F3）。三个取值都在冻结全集内。
func progressForStatus(status Status) protocol.ProgressStatus {
	switch status {
	case StatusMissing:
		return protocol.ProgressSkipped
	case StatusError:
		return protocol.ProgressFailed
	default:
		return protocol.ProgressSucceeded
	}
}

func summarize(checks []Check) Summary {
	summary := Summary{Total: len(checks)}
	for _, check := range checks {
		switch check.Status {
		case StatusOK:
			summary.OK++
		case StatusMissing:
			summary.Missing++
		case StatusError:
			summary.Error++
		}
	}
	return summary
}

func (s *Service) checkAppRoot(ctx context.Context) Check {
	return checkDirectory(ctx, "app-root", "应用根目录", s.layout.AppRoot())
}

func (s *Service) checkLayout(ctx context.Context) Check {
	paths := map[string]string{
		"repo":          s.layout.RepoDir(),
		"runtime-state": s.layout.StateDir(),
		"runtime":       s.layout.RuntimeDir(),
		"logs":          s.layout.LogsDir(),
	}
	details := make(map[string]any, len(paths))
	worst := StatusOK
	missingCount := 0
	for name, path := range paths {
		info, err := os.Stat(path)
		switch {
		case err == nil && info.IsDir():
			details[name] = "ok"
		case errors.Is(err, fs.ErrNotExist):
			details[name] = "missing"
			missingCount++
			worst = worseStatus(worst, StatusMissing)
		case err != nil:
			details[name] = "error"
			worst = StatusError
		default:
			details[name] = "error"
			worst = StatusError
		}
	}
	message := fmt.Sprintf("受管目录 %d 个中 %d 个缺失", len(paths), missingCount)
	if worst == StatusError {
		message = "受管目录存在类型异常"
	}
	return Check{
		ID:      "layout",
		Name:    "受管目录布局",
		Status:  worst,
		Message: message,
		Details: details,
	}
}

func (s *Service) checkUV(ctx context.Context) Check {
	entries, err := os.ReadDir(s.layout.UVToolsDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Check{
				ID:      "uv",
				Name:    "uv 工具",
				Status:  StatusMissing,
				Message: "未安装受管 uv",
				Details: map[string]any{},
			}
		}
		return Check{
			ID:      "uv",
			Name:    "uv 工具",
			Status:  StatusError,
			Message: "无法访问 uv 工具目录",
			Details: map[string]any{"error": errorKind(err)},
		}
	}
	executables := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		exe, err := s.layout.UVExecutable(entry.Name())
		if err != nil {
			continue
		}
		if info, err := os.Stat(exe); err == nil && !info.IsDir() {
			executables = append(executables, exe)
		}
	}
	sort.Strings(executables)
	if len(executables) == 0 {
		return Check{
			ID:      "uv",
			Name:    "uv 工具",
			Status:  StatusMissing,
			Message: "未找到受管 uv.exe",
			Details: map[string]any{},
		}
	}
	var lastErr error
	for _, exe := range executables {
		version, err := s.probes.UVVersion(ctx, exe)
		if err != nil {
			lastErr = err
			continue
		}
		return Check{
			ID:      "uv",
			Name:    "uv 工具",
			Status:  StatusOK,
			Message: "uv 可用",
			Details: map[string]any{"version": version},
		}
	}
	return Check{
		ID:      "uv",
		Name:    "uv 工具",
		Status:  StatusError,
		Message: "无法识别 uv 版本",
		Details: map[string]any{"error": errorKind(lastErr)},
	}
}

func (s *Service) checkPython(ctx context.Context) Check {
	info, err := os.Stat(s.layout.PythonDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Check{
				ID:      "python",
				Name:    "受管 Python",
				Status:  StatusMissing,
				Message: "未安装受管 Python",
				Details: map[string]any{},
			}
		}
		return errorCheck("python", "受管 Python", "无法访问 Python 目录", err)
	}
	if !info.IsDir() {
		return Check{
			ID:      "python",
			Name:    "受管 Python",
			Status:  StatusError,
			Message: "Python 路径不是目录",
			Details: map[string]any{},
		}
	}
	exe := s.layout.PythonExecutable()
	if fileInfo, err := os.Stat(exe); err != nil || fileInfo.IsDir() {
		return Check{
			ID:      "python",
			Name:    "受管 Python",
			Status:  StatusMissing,
			Message: "未找到受管 python.exe",
			Details: map[string]any{},
		}
	}
	return Check{
		ID:      "python",
		Name:    "受管 Python",
		Status:  StatusOK,
		Message: "Python 目录存在",
		Details: map[string]any{},
	}
}

func (s *Service) checkRepo(ctx context.Context) Check {
	repoInfo, err := os.Stat(s.layout.RepoDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Check{
				ID:      "repo",
				Name:    "受管仓库",
				Status:  StatusMissing,
				Message: "repo 目录不存在",
				Details: map[string]any{},
			}
		}
		return errorCheck("repo", "受管仓库", "无法访问 repo 目录", err)
	}
	if !repoInfo.IsDir() {
		return Check{
			ID:      "repo",
			Name:    "受管仓库",
			Status:  StatusError,
			Message: "repo 路径不是目录",
			Details: map[string]any{},
		}
	}
	versionFile := s.layout.RepoVersionFile()
	data, err := readBounded(versionFile, maxVersionFileBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Check{
				ID:      "repo",
				Name:    "受管仓库",
				Status:  StatusMissing,
				Message: "版本文件缺失",
				Details: map[string]any{},
			}
		}
		return errorCheck("repo", "受管仓库", "无法读取版本文件", err)
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return Check{
			ID:      "repo",
			Name:    "受管仓库",
			Status:  StatusError,
			Message: "版本文件不是有效 JSON",
			Details: map[string]any{
				"error": errorKind(fmt.Errorf("%w: %v", errMalformedPayload, err)),
			},
		}
	}
	if payload.Version == "" {
		return Check{
			ID:      "repo",
			Name:    "受管仓库",
			Status:  StatusError,
			Message: "版本文件缺少 version 字段",
			Details: map[string]any{},
		}
	}
	return Check{
		ID:      "repo",
		Name:    "受管仓库",
		Status:  StatusOK,
		Message: "仓库与版本文件存在",
		Details: map[string]any{"version": payload.Version},
	}
}

func (s *Service) checkVenv(ctx context.Context) Check {
	return checkDirectory(ctx, "venv", "主项目虚拟环境", s.layout.VenvDir())
}

func (s *Service) checkRuntimeState(ctx context.Context) Check {
	files := map[string]string{
		"backend":     s.layout.BackendStateFile(),
		"mutation":    s.layout.MutationStateFile(),
		"update":      s.layout.UpdateStateFile(),
		"environment": s.layout.EnvironmentStateFile(),
	}
	details := make(map[string]any, len(files))
	worst := StatusOK
	for name, path := range files {
		data, err := readBounded(path, maxStateFileCheckBytes)
		switch {
		case err == nil && isJSONObject(data):
			details[name] = "ok"
		case errors.Is(err, fs.ErrNotExist):
			details[name] = "missing"
			worst = worseStatus(worst, StatusMissing)
		case err != nil:
			details[name] = "error"
			worst = StatusError
		default:
			details[name] = "error"
			worst = StatusError
		}
	}
	message := "runtime-state 文件完整"
	if worst == StatusMissing {
		message = "runtime-state 存在缺失文件"
	} else if worst == StatusError {
		message = "runtime-state 存在损坏或超限文件"
	}
	return Check{
		ID:      "runtime-state",
		Name:    "运行时状态文件",
		Status:  worst,
		Message: message,
		Details: details,
	}
}

func (s *Service) checkMutexes(ctx context.Context) Check {
	set, err := lock.NewSet(ctx, s.layout)
	if err != nil {
		return errorCheck("mutex", "并发锁占用", "无法初始化锁探测", err)
	}
	backend, err := set.Probe(ctx, lock.KindBackend)
	if err != nil {
		return errorCheck(
			"mutex",
			"并发锁占用",
			"backend 锁探测失败",
			errors.Join(err, set.Close()),
		)
	}
	mutation, err := set.Probe(ctx, lock.KindMutation)
	if err != nil {
		return errorCheck(
			"mutex",
			"并发锁占用",
			"mutation 锁探测失败",
			errors.Join(err, set.Close()),
		)
	}
	details := map[string]any{
		"backend":           backend.Held,
		"backendRecovered":  backend.Recovered,
		"mutation":          mutation.Held,
		"mutationRecovered": mutation.Recovered,
	}
	if closeErr := set.Close(); closeErr != nil {
		// 探测已成功，仅收口失败：把故障计入该项，避免忽略资源清理错误。
		return errorCheck("mutex", "并发锁占用", "锁探测收口失败", closeErr)
	}
	return Check{
		ID:      "mutex",
		Name:    "并发锁占用",
		Status:  StatusOK,
		Message: "锁占用探测完成",
		Details: details,
	}
}

func (s *Service) checkDisk(ctx context.Context) Check {
	free, err := s.probes.DiskFree(ctx, s.layout.AppRoot())
	if err != nil {
		return errorCheck("disk", "磁盘剩余空间", "磁盘探测失败", err)
	}
	return Check{
		ID:      "disk",
		Name:    "磁盘剩余空间",
		Status:  StatusOK,
		Message: "磁盘剩余空间可用",
		Details: map[string]any{"freeBytes": free},
	}
}

func checkDirectory(ctx context.Context, id, name, path string) Check {
	info, err := os.Stat(path)
	switch {
	case err == nil && info.IsDir():
		return Check{
			ID:      id,
			Name:    name,
			Status:  StatusOK,
			Message: "目录存在",
			Details: map[string]any{},
		}
	case errors.Is(err, fs.ErrNotExist):
		return Check{
			ID:      id,
			Name:    name,
			Status:  StatusMissing,
			Message: "目录不存在",
			Details: map[string]any{},
		}
	case err != nil:
		return errorCheck(id, name, "无法访问目录", err)
	default:
		return Check{
			ID:      id,
			Name:    name,
			Status:  StatusError,
			Message: "路径不是目录",
			Details: map[string]any{},
		}
	}
}

func errorCheck(id, name, message string, cause error) Check {
	return Check{
		ID:      id,
		Name:    name,
		Status:  StatusError,
		Message: message,
		Details: map[string]any{"error": errorKind(cause)},
	}
}

// errorKind 把底层错误映射为 doctor 检查项 details 的稳定分类词。
// wire 上的 details 只允许稳定分类词，原始错误串只能进入 stderr 诊断或日志。
func errorKind(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not-found"
	case errors.Is(err, fs.ErrPermission):
		return "access-denied"
	case errors.Is(err, errMalformedPayload), errors.Is(err, os.ErrInvalid):
		return "invalid"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	default:
		return "other"
	}
}

func readBounded(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%w (limit %d)", errStateFileTooLarge, limit)
	}
	return os.ReadFile(path)
}

func isJSONObject(data []byte) bool {
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return false
	}
	return object != nil
}

func worseStatus(left, right Status) Status {
	order := map[Status]int{StatusOK: 0, StatusMissing: 1, StatusError: 2}
	if order[right] > order[left] {
		return right
	}
	return left
}
