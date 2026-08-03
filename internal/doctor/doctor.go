package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// 分派表要求统一签名，因此不需要 ctx 的检查函数也保留该参数（写成匿名参数）。
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
			return Report{}, newOutputError(err)
		}
		checks = append(checks, check)
	}
	summary := summarize(checks)
	if err := emitSummary(emitter, summary); err != nil {
		return Report{}, newOutputError(err)
	}
	return Report{Checks: checks, Summary: summary}, nil
}

// newOutputError 把协议事件发射失败映射为 OUTPUT_WRITE_FAILED。
// 显式映射而不是交给 cli 的未知错误兜底：兜底码已改为 INTERNAL_ERROR
// （T3.8 F13d），输出通道故障不该被报成 Runtime 内部缺陷。
func newOutputError(cause error) *Error {
	return NewError(
		protocol.CodeOutputWriteFailed,
		protocol.StageDoctor,
		"诊断事件发射失败",
		map[string]any{},
		cause,
	)
}

func emitCheck(emitter *protocol.Emitter, check Check) error {
	if err := emitter.EmitProgress(protocol.ProgressEvent{
		Stage:   protocol.StageDoctor,
		Status:  protocol.ProgressRunning,
		Message: check.Name,
	}); err != nil {
		return err
	}
	return emitter.EmitProgress(protocol.ProgressEvent{
		Stage:   protocol.StageDoctor,
		Status:  progressForStatus(check.Status),
		Message: fmt.Sprintf("%s：%s", check.Name, check.Message),
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

// checkSpec 是一个检查项的稳定身份：wire 上的 id 与 human 输出用的中文名。
// 每个检查项在这里出现一次，检查函数只描述判定，不再重复 id/name 字面量。
type checkSpec struct {
	id   string
	name string
}

var (
	specAppRoot      = checkSpec{id: "app-root", name: "应用根目录"}
	specLayout       = checkSpec{id: "layout", name: "受管目录布局"}
	specUV           = checkSpec{id: "uv", name: "uv 工具"}
	specPython       = checkSpec{id: "python", name: "受管 Python"}
	specRepo         = checkSpec{id: "repo", name: "受管仓库"}
	specVenv         = checkSpec{id: "venv", name: "主项目虚拟环境"}
	specRuntimeState = checkSpec{id: "runtime-state", name: "运行时状态文件"}
	specMutex        = checkSpec{id: "mutex", name: "并发锁占用"}
	specDisk         = checkSpec{id: "disk", name: "磁盘剩余空间"}
)

// result 构造该检查项的结果；details 为 nil 时归一化为空 map，
// 保证 wire 上的 details 恒为对象而非 null。
func (s checkSpec) result(status Status, message string, details map[string]any) Check {
	if details == nil {
		details = map[string]any{}
	}
	return Check{ID: s.id, Name: s.name, Status: status, Message: message, Details: details}
}

func (s checkSpec) ok(message string, details map[string]any) Check {
	return s.result(StatusOK, message, details)
}

func (s checkSpec) missing(message string) Check {
	return s.result(StatusMissing, message, nil)
}

// failed 报告确定的异常，details 为空：原因已由 message 表达。
func (s checkSpec) failed(message string) Check {
	return s.result(StatusError, message, nil)
}

// failedBecause 报告由底层错误引起的异常，details 只放稳定分类词。
// 原始错误串绝不进 wire（T3.5 F6），只能进 stderr 诊断或日志。
func (s checkSpec) failedBecause(message string, cause error) Check {
	return s.result(StatusError, message, map[string]any{"error": errorKind(cause)})
}

// directory 是「目录存在且是目录」这一最常见判定的共用实现。
func (s checkSpec) directory(path string) Check {
	info, err := os.Stat(path)
	switch {
	case err == nil && info.IsDir():
		return s.ok("目录存在", nil)
	case errors.Is(err, fs.ErrNotExist):
		return s.missing("目录不存在")
	case err != nil:
		return s.failedBecause("无法访问目录", err)
	default:
		return s.failed("路径不是目录")
	}
}

func (s *Service) checkAppRoot(context.Context) Check {
	return specAppRoot.directory(s.layout.AppRoot())
}

func (s *Service) checkVenv(context.Context) Check {
	return specVenv.directory(s.layout.VenvDir())
}

func (s *Service) checkLayout(context.Context) Check {
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
		default:
			details[name] = "error"
			worst = StatusError
		}
	}
	if worst == StatusError {
		return specLayout.result(worst, "受管目录存在类型异常", details)
	}
	return specLayout.result(
		worst,
		fmt.Sprintf("受管目录 %d 个中 %d 个缺失", len(paths), missingCount),
		details,
	)
}

func (s *Service) checkUV(ctx context.Context) Check {
	entries, err := os.ReadDir(s.layout.UVToolsDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return specUV.missing("未安装受管 uv")
		}
		return specUV.failedBecause("无法访问 uv 工具目录", err)
	}
	executables := s.uvExecutables(entries)
	if len(executables) == 0 {
		return specUV.missing("未找到受管 uv.exe")
	}
	// 整项总预算：单次探测已有固定超时，但残留的历史版本目录会让 N 个候选
	// 串行消耗 N 倍时间。doctor 是只读诊断，整项超出一次探测的预算后继续
	// 枚举没有价值（T3.8 F13c）。
	probeCtx, cancel := context.WithTimeout(ctx, probeUVBudget)
	defer cancel()
	var lastErr error
	for _, exe := range executables {
		version, err := s.probes.UVVersion(probeCtx, exe)
		if err != nil {
			lastErr = err
			if probeCtx.Err() != nil {
				break
			}
			continue
		}
		return specUV.ok("uv 可用", map[string]any{"version": version})
	}
	return specUV.failedBecause("无法识别 uv 版本", lastErr)
}

// uvExecutables 返回受管 uv 版本目录下存在的 uv.exe，按路径排序保证结果稳定。
func (s *Service) uvExecutables(entries []os.DirEntry) []string {
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
	return executables
}

func (s *Service) checkPython(context.Context) Check {
	info, err := os.Stat(s.layout.PythonDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return specPython.missing("未安装受管 Python")
		}
		return specPython.failedBecause("无法访问 Python 目录", err)
	}
	if !info.IsDir() {
		return specPython.failed("Python 路径不是目录")
	}
	if fileInfo, err := os.Stat(s.layout.PythonExecutable()); err != nil || fileInfo.IsDir() {
		return specPython.missing("未找到受管 python.exe")
	}
	return specPython.ok("Python 目录存在", nil)
}

func (s *Service) checkRepo(context.Context) Check {
	repoInfo, err := os.Stat(s.layout.RepoDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return specRepo.missing("repo 目录不存在")
		}
		return specRepo.failedBecause("无法访问 repo 目录", err)
	}
	if !repoInfo.IsDir() {
		return specRepo.failed("repo 路径不是目录")
	}
	data, err := readBounded(s.layout.RepoVersionFile(), maxVersionFileBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return specRepo.missing("版本文件缺失")
		}
		return specRepo.failedBecause("无法读取版本文件", err)
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return specRepo.failedBecause(
			"版本文件不是有效 JSON",
			fmt.Errorf("%w: %v", errMalformedPayload, err),
		)
	}
	if payload.Version == "" {
		return specRepo.failed("版本文件缺少 version 字段")
	}
	return specRepo.ok("仓库与版本文件存在", map[string]any{"version": payload.Version})
}

func (s *Service) checkRuntimeState(context.Context) Check {
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
		default:
			details[name] = "error"
			worst = StatusError
		}
	}
	message := "runtime-state 文件完整"
	switch worst {
	case StatusMissing:
		message = "runtime-state 存在缺失文件"
	case StatusError:
		message = "runtime-state 存在损坏或超限文件"
	}
	return specRuntimeState.result(worst, message, details)
}

func (s *Service) checkMutexes(ctx context.Context) Check {
	set, err := lock.NewSet(ctx, s.layout)
	if err != nil {
		return specMutex.failedBecause("无法初始化锁探测", err)
	}
	backend, err := set.Probe(ctx, lock.KindBackend)
	if err != nil {
		return specMutex.failedBecause("backend 锁探测失败", errors.Join(err, set.Close()))
	}
	mutation, err := set.Probe(ctx, lock.KindMutation)
	if err != nil {
		return specMutex.failedBecause("mutation 锁探测失败", errors.Join(err, set.Close()))
	}
	details := map[string]any{
		"backend":           backend.Held,
		"backendRecovered":  backend.Recovered,
		"mutation":          mutation.Held,
		"mutationRecovered": mutation.Recovered,
	}
	if closeErr := set.Close(); closeErr != nil {
		// 探测已成功，仅收口失败：把故障计入该项，避免忽略资源清理错误。
		return specMutex.failedBecause("锁探测收口失败", closeErr)
	}
	return specMutex.ok("锁占用探测完成", details)
}

func (s *Service) checkDisk(ctx context.Context) Check {
	free, err := s.probes.DiskFree(ctx, s.layout.AppRoot())
	if err != nil {
		return specDisk.failedBecause("磁盘探测失败", err)
	}
	return specDisk.ok("磁盘剩余空间可用", map[string]any{"freeBytes": free})
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

// readBounded 读取至多 limit 字节；超过上限即报错。
//
// 用 io.LimitReader 而不是先 Stat 再 ReadFile：后者的上限只是咨询值，
// Stat 与实际读取之间文件仍可增长（T3.8 F13b）。多读一个字节是为了区分
// 「恰好等于上限」与「超过上限」。
func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w (limit %d)", errStateFileTooLarge, limit)
	}
	return data, nil
}

func isJSONObject(data []byte) bool {
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return false
	}
	return object != nil
}

// statusOrder 决定 worseStatus 的严重度顺序；提为包级避免每次比较重建 map。
var statusOrder = map[Status]int{StatusOK: 0, StatusMissing: 1, StatusError: 2}

func worseStatus(left, right Status) Status {
	if statusOrder[right] > statusOrder[left] {
		return right
	}
	return left
}
