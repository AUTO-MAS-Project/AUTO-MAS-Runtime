package cleanup

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/lock"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/state"
)

// ItemStatus 是单条清理目标的稳定结果。
type ItemStatus string

const (
	ItemCleaned ItemStatus = "cleaned"
	ItemSkipped ItemStatus = "skipped"
	ItemFailed  ItemStatus = "failed"
)

// Valid 报告 ItemStatus 是否属于稳定全集。
func (s ItemStatus) Valid() bool {
	switch s {
	case ItemCleaned, ItemSkipped, ItemFailed:
		return true
	default:
		return false
	}
}

// String 返回 ItemStatus 的稳定字面量。
func (s ItemStatus) String() string { return string(s) }

// Item 描述一次清理目标的稳定结果。
type Item struct {
	ID      string
	Status  ItemStatus
	Message string
	Details map[string]any
}

// Summary 汇总一次清理的计数。
type Summary struct {
	Total   int
	Cleaned int
	Skipped int
	Failed  int
}

// Report 是 cleanup 的结构化输出，全部字段都不含绝对路径。
type Report struct {
	Items   []Item
	Summary Summary
}

// ReportDetails 返回 result.details 使用的非敏感条目清单与计数。
func ReportDetails(report Report) map[string]any {
	items := make([]map[string]any, 0, len(report.Items))
	for _, item := range report.Items {
		details := item.Details
		if details == nil {
			details = map[string]any{}
		}
		items = append(items, map[string]any{
			"id":      item.ID,
			"status":  item.Status.String(),
			"message": item.Message,
			"details": details,
		})
	}
	return map[string]any{
		"items": items,
		"summary": map[string]any{
			"total":   report.Summary.Total,
			"cleaned": report.Summary.Cleaned,
			"skipped": report.Summary.Skipped,
			"failed":  report.Summary.Failed,
		},
	}
}

// Service 组合布局与审计器执行清理。
type Service struct {
	layout  *config.Layout
	auditor filesystem.Auditor
}

// New 创建清理服务，layout 与 auditor 都不可为空。
func New(layout *config.Layout, auditor filesystem.Auditor) (*Service, error) {
	if layout == nil || auditor == nil {
		return nil, errors.New("cleanup service requires layout and auditor")
	}
	return &Service{layout: layout, auditor: auditor}, nil
}

// planItem 是清理计划中的单条目标，只有两种互斥形态：
//
//   - 待删除目标（newDeleteItem）：携带 kind/target/operationID/reason；
//     可清理的 repo.update-* 还携带枚举时取得的 expectedIdentity，
//     由 executeItem 交给 filesystem.Operator 执行；
//   - 已定结果（newResolvedItem）：枚举阶段就已判定 skipped/failed，
//     只携带 resolved，不再触碰文件系统。
//
// 用 resolved 指针表达这个二选一，避免八个字段互斥两组时的非法状态可表达
// （例如同时带 target 和 preStatus）。两种构造函数是唯一入口。
type planItem struct {
	id               string
	kind             filesystem.DeleteKind
	target           string
	operationID      string
	reason           string
	expectedIdentity *filesystem.DirectoryIdentity
	resolved         *resolvedItem
}

// resolvedItem 是枚举阶段就已确定的条目结果。
type resolvedItem struct {
	status  ItemStatus
	message string
	details map[string]any
}

// newDeleteItem 构造一条待删除目标。
func newDeleteItem(id string, kind filesystem.DeleteKind, target, operationID string) planItem {
	return newDeleteItemWithIdentity(id, kind, target, operationID, nil)
}

// newDeleteItemWithIdentity 构造带目录身份凭据的待删除目标。
//
// 只有枚举阶段已证明目录身份的 repo.update-* 才传入 token；缓存条目保持
// nil，交由 filesystem.Operator 按其固定布局规则重新授权。
func newDeleteItemWithIdentity(
	id string,
	kind filesystem.DeleteKind,
	target string,
	operationID string,
	expectedIdentity *filesystem.DirectoryIdentity,
) planItem {
	return planItem{
		id:               id,
		kind:             kind,
		target:           target,
		operationID:      operationID,
		reason:           cleanupReason,
		expectedIdentity: expectedIdentity,
	}
}

// newResolvedItem 构造一条枚举阶段已定结果的条目。
func newResolvedItem(id string, status ItemStatus, message string, details map[string]any) planItem {
	return planItem{
		id:       id,
		resolved: &resolvedItem{status: status, message: message, details: details},
	}
}

// newFailedItem 构造一条失败关闭条目，details 固定复述 GIT_REPO_CLEANUP_FAILED。
func newFailedItem(id, message string) planItem {
	return newResolvedItem(
		id,
		ItemFailed,
		message,
		map[string]any{"code": string(protocol.CodeGitRepoCleanupFailed)},
	)
}

// cleanupReason 是本命令写入删除审计的固定原因标识。
const cleanupReason = "cleanup"

// Run 获取 mutation 锁后执行清理计划。
// 任一条目失败时命令返回 GIT_REPO_CLEANUP_FAILED，details 保留完整副作用报告。
func (s *Service) Run(ctx context.Context, emitter *protocol.Emitter) (report Report, err error) {
	if err := ctx.Err(); err != nil {
		return Report{}, newCancelledError(map[string]any{}, err)
	}
	if err := s.requireAppRoot(); err != nil {
		return Report{}, err
	}
	set, err := lock.NewSet(ctx, s.layout)
	if err != nil {
		return Report{}, mapLockError(err)
	}
	defer func() {
		if closeErr := set.Close(); closeErr != nil {
			err = errors.Join(err, mapLockError(closeErr))
		}
	}()
	acquisition, err := set.AcquireMutation(ctx)
	if err != nil {
		return Report{}, mapLockError(err)
	}
	defer func() {
		if closeErr := acquisition.Lease().Close(); closeErr != nil {
			err = errors.Join(err, mapLockError(closeErr))
		}
	}()
	if err := ctx.Err(); err != nil {
		return Report{}, newCancelledError(map[string]any{}, err)
	}
	operator, err := filesystem.New(ctx, s.layout, s.auditor)
	if err != nil {
		return Report{}, mapOperationError(err, protocol.CodeGitRepoCleanupFailed, "无法初始化安全删除能力")
	}
	plan, err := s.buildPlan(ctx, emitter.OperationID())
	if err != nil {
		return Report{}, mapOperationError(err, protocol.CodeGitRepoCleanupFailed, "无法生成清理计划")
	}
	report = Report{Items: make([]Item, 0, len(plan))}
	for _, entry := range plan {
		if err := ctx.Err(); err != nil {
			report.Summary = summarize(report.Items)
			return report, newCancelledError(ReportDetails(report), err)
		}
		item, err := s.executeItem(ctx, emitter, operator, entry)
		if err != nil {
			report.Summary = summarize(report.Items)
			return report, err
		}
		report.Items = append(report.Items, item)
	}
	report.Summary = summarize(report.Items)
	for _, item := range report.Items {
		if item.Status == ItemFailed {
			return report, NewError(
				protocol.CodeGitRepoCleanupFailed,
				protocol.StageCleanup,
				"部分清理项目失败",
				ReportDetails(report),
				errors.New("one or more cleanup items failed"),
			)
		}
	}
	return report, nil
}

// requireAppRoot 在取锁前做只读前置校验：app-root 必须存在且是目录。
//
// 不做这一步时，lock.NewSet 打不开根目录句柄，错误被 mapLockError 兜底成
// MUTEX_OPERATION_FAILED（exit 70、retryable=true、remediation=[retry,
// run-doctor]）。真实原因是调用方给的 --app-root 指向不存在的目录，重试
// 永远不会成功，而 retryable=true 会驱动 Electron 陷入无意义重试循环
// （T3.7 F4）。只有「不存在」与「存在但非目录」两种可确定归因于参数的情形
// 走这里，其余 stat 错误（如权限）保持既有 MUTEX_OPERATION_FAILED 路径。
func (s *Service) requireAppRoot() error {
	info, err := os.Stat(s.layout.AppRoot())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return newInvalidAppRootError("应用根目录不存在", err)
	case err != nil:
		return nil
	case !info.IsDir():
		return newInvalidAppRootError(
			"应用根目录不是目录",
			errors.New("app root is not a directory"),
		)
	default:
		return nil
	}
}

func newInvalidAppRootError(message string, cause error) error {
	return NewError(
		protocol.CodeInvalidArgument,
		protocol.StageCleanup,
		message,
		map[string]any{},
		cause,
	)
}

func (s *Service) buildPlan(ctx context.Context, cleanupOperationID string) ([]planItem, error) {
	items := []planItem{
		newDeleteItem("uv-cache", filesystem.DeleteUVCache, s.layout.UVCacheDir(), cleanupOperationID),
		newDeleteItem("download-temp", filesystem.DeleteDownloadTemporary, s.layout.DownloadCacheDir(), cleanupOperationID),
		newDeleteItem("build-cache", filesystem.DeleteBuildCache, s.layout.BuildCacheDir(), cleanupOperationID),
	}
	pythonItems, err := s.enumeratePythonCaches(ctx, cleanupOperationID)
	if err != nil {
		return nil, err
	}
	items = append(items, pythonItems...)
	updateItems, err := s.enumerateRepoUpdates(ctx)
	if err != nil {
		return nil, err
	}
	items = append(items, updateItems...)
	return items, nil
}

func (s *Service) executeItem(
	ctx context.Context,
	emitter *protocol.Emitter,
	operator *filesystem.Operator,
	entry planItem,
) (Item, error) {
	if entry.resolved != nil {
		if err := emitItemProgress(
			emitter,
			entry.id,
			progressForStatus(entry.resolved.status),
			entry.resolved.message,
		); err != nil {
			return Item{}, err
		}
		return Item{
			ID:      entry.id,
			Status:  entry.resolved.status,
			Message: entry.resolved.message,
			Details: entry.resolved.details,
		}, nil
	}
	if err := emitItemProgress(
		emitter,
		entry.id,
		protocol.ProgressRunning,
		"正在清理 "+entry.id,
	); err != nil {
		return Item{}, err
	}
	result, err := operator.RemoveTree(ctx, filesystem.DeleteRequest{
		Kind:             entry.kind,
		Target:           entry.target,
		OperationID:      entry.operationID,
		Reason:           entry.reason,
		ExpectedIdentity: entry.expectedIdentity,
	})
	if err != nil {
		code := operationErrorCode(err, protocol.CodeGitRepoCleanupFailed)
		item := Item{
			ID:      entry.id,
			Status:  ItemFailed,
			Message: messageForItemFailure(code),
			Details: map[string]any{"code": string(code)},
		}
		if progressErr := emitItemProgress(
			emitter,
			entry.id,
			protocol.ProgressFailed,
			item.Message,
		); progressErr != nil {
			return Item{}, progressErr
		}
		return item, nil
	}
	// Partial 只在 RemoveTree 返回错误时成立（已走 failed 分支），
	// 因此成功返回后 !Removed 即代表目标不存在，跳过即可。
	if !result.Removed {
		item := Item{
			ID:      entry.id,
			Status:  ItemSkipped,
			Message: "目标不存在",
			Details: map[string]any{},
		}
		if progressErr := emitItemProgress(
			emitter,
			entry.id,
			protocol.ProgressSkipped,
			item.Message,
		); progressErr != nil {
			return Item{}, progressErr
		}
		return item, nil
	}
	item := Item{
		ID:      entry.id,
		Status:  ItemCleaned,
		Message: "已清理",
		Details: map[string]any{},
	}
	if progressErr := emitItemProgress(
		emitter,
		entry.id,
		protocol.ProgressSucceeded,
		item.Message,
	); progressErr != nil {
		return Item{}, progressErr
	}
	return item, nil
}

// isLinkDirEntry 报告目录项是否是不可跟随的链接目录（Junction 或符号链接）。
//
// Windows 的 Junction 在 Lstat/DirEntry 中表现为 ModeIrregular 且 IsDir()=false
// （Go 对 IO_REPARSE_TAG_MOUNT_POINT 的分类），符号链接则是 ModeSymlink。
// 任何只靠 IsDir() 的过滤都会把 Junction 当成普通文件跳过——这正是 T3.6 F2
// 与 T3.7 F2 两次缺陷的共同根因，因此判定收敛到这一个函数，两处枚举共用。
func isLinkDirEntry(entry fs.DirEntry) bool {
	mode := entry.Type()
	if mode&os.ModeIrregular != 0 {
		return true
	}
	return mode&os.ModeSymlink != 0 && entry.IsDir()
}

// invalidLinkItem 构造「链接目录不跟随、不删除」的失败关闭条目。
// id 形如 <prefix>-invalid-<n>，保证 result.details.items 可寻址。
func invalidLinkItem(prefix string, counter int) planItem {
	return newFailedItem(prefix+"-invalid-"+strconv.Itoa(counter), "目录为不安全的链接")
}

// enumeratePythonCaches 在 repo 下寻找 __pycache__ 目录。
// 候选目录先经 filesystem.Canonicalize 验证普通目录身份；名为 __pycache__
// 的 Junction/符号链接目录不跟随、不删除，记为 failed 条目；非 __pycache__
// 的 symlink/junction 目录保持静默跳过；遍历不跟随任何重解析点。
func (s *Service) enumeratePythonCaches(ctx context.Context, cleanupOperationID string) ([]planItem, error) {
	repo := s.layout.RepoDir()
	if _, err := os.Stat(repo); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var items []planItem
	counter := 0
	invalidCounter := 0
	err := filepath.WalkDir(repo, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if isLinkDirEntry(entry) {
			if entry.Name() == "__pycache__" {
				// 名为 __pycache__ 的 Junction/符号链接目录：不跟随、不删除，
				// 按失败关闭语义记为 failed 条目。
				invalidCounter++
				items = append(items, invalidLinkItem("python-cache", invalidCounter))
			}
			// 不跟随链接目录。只有 DirEntry 自报为目录时才用 SkipDir 阻止下降：
			// 对 Junction（IsDir()=false）返回 SkipDir 会被 WalkDir 解释成
			// 「跳过所在目录的剩余条目」，把排在它后面的真实 __pycache__ 一并
			// 吞掉；WalkDir 本来就不会下降到非目录条目，返回 nil 即可。
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() && entry.Name() == "__pycache__" {
			counter++
			id := "python-cache-" + strconv.Itoa(counter)
			if _, err := filesystem.Canonicalize(path); err != nil {
				items = append(items, newFailedItem(id, "目录身份不明确"))
				return fs.SkipDir
			}
			items = append(items, newDeleteItem(
				id,
				filesystem.DeletePythonCache,
				path,
				cleanupOperationID,
			))
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

// enumerateRepoUpdates 枚举 app-root 下的 repo.update-* 目录并做状态身份匹配。
func (s *Service) enumerateRepoUpdates(ctx context.Context) ([]planItem, error) {
	entries, err := os.ReadDir(s.layout.AppRoot())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var items []planItem
	invalidCounter := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "repo.update-") {
			continue
		}
		if isLinkDirEntry(entry) {
			// 名为 repo.update-* 的 Junction/符号链接：不跟随、不删除，记为
			// failed 条目。这类目录名声称自己是受管更新暂存目录，静默跳过等于
			// 对「危险路径（Junction 指向用户数据）拒绝」给出成功答案（T3.7 F2）。
			invalidCounter++
			items = append(items, invalidLinkItem("repo-update", invalidCounter))
			continue
		}
		if !entry.IsDir() {
			continue
		}
		operationID := strings.TrimPrefix(entry.Name(), "repo.update-")
		expected, err := s.layout.RepoUpdateDir(operationID)
		if err != nil || filepath.Base(expected) != entry.Name() {
			// id 必须唯一且稳定（与 python-cache-<n> 同风格），
			// 唯一 id 是 result.details.items 可寻址的前提；
			// 枚举顺序由 os.ReadDir 的字典序保证稳定。
			invalidCounter++
			items = append(items, newFailedItem(
				"repo-update-invalid-"+strconv.Itoa(invalidCounter),
				"目录身份不明确",
			))
			continue
		}
		inspection, inspectErr := filesystem.InspectManagedDirectory(ctx, s.layout, expected)
		if inspectErr != nil {
			if errors.Is(inspectErr, context.Canceled) || errors.Is(inspectErr, context.DeadlineExceeded) {
				return nil, inspectErr
			}
			invalidCounter++
			items = append(items, newFailedItem(
				"repo-update-invalid-"+strconv.Itoa(invalidCounter),
				"目录身份不明确",
			))
			continue
		}
		if !inspection.Exists || inspection.Identity == nil {
			invalidCounter++
			items = append(items, newFailedItem(
				"repo-update-invalid-"+strconv.Itoa(invalidCounter),
				"目录身份不明确",
			))
			continue
		}
		id := "repo-update-" + operationID
		cleanable, outcome, err := s.classifyRepoUpdate(ctx, operationID)
		switch {
		case err != nil:
			items = append(items, newFailedItem(id, "状态校验失败"))
		case cleanable:
			items = append(items, newDeleteItemWithIdentity(
				id,
				filesystem.DeleteRepositoryUpdate,
				expected,
				operationID,
				inspection.Identity,
			))
		case outcome == outcomeActive:
			items = append(items, newResolvedItem(id, ItemSkipped, "更新仍在进行", nil))
		default:
			items = append(items, newFailedItem(id, messageForRepoUpdateOutcome(outcome)))
		}
	}
	return items, nil
}

// repoUpdateOutcome 是 repo.update-* 身份判定的稳定分类。
// 只用于选择条目文案，不出现在 wire 上。
type repoUpdateOutcome string

const (
	outcomeNoTransaction    repoUpdateOutcome = "no-transaction"
	outcomeIdentityMismatch repoUpdateOutcome = "identity-mismatch"
	outcomeInconsistent     repoUpdateOutcome = "inconsistent"
	outcomeActive           repoUpdateOutcome = "active"
	outcomeStale            repoUpdateOutcome = "stale"
	outcomeStoreError       repoUpdateOutcome = "store-error"
	outcomeReadError        repoUpdateOutcome = "read-error"
	outcomeUnknown          repoUpdateOutcome = "unknown"
)

// messageForRepoUpdateOutcome 返回失败关闭条目的中文原因。
// 未列出的分类统一按「状态身份不匹配」处理：失败关闭不需要区分到底哪种不明。
func messageForRepoUpdateOutcome(outcome repoUpdateOutcome) string {
	switch outcome {
	case outcomeNoTransaction:
		return "缺少更新事务"
	case outcomeIdentityMismatch:
		return "目录身份与事务不匹配"
	case outcomeInconsistent:
		return "事务 PID 仍存活"
	default:
		return "状态身份不匹配"
	}
}

// classifyRepoUpdate 读取 update 事务并判定是否可安全清理。
// cleanup 已持有 mutation 租约，任何 peer 都不可能持有同一 Mutex，
// 因此对端探测恒为 free，活动性由 PID 探针区分 stale/inconsistent。
func (s *Service) classifyRepoUpdate(
	ctx context.Context,
	operationID string,
) (bool, repoUpdateOutcome, error) {
	// 无副作用的只读探测：更新状态文件不存在即视为无事务，失败关闭且
	// 不打开 StateFiles，避免创建 runtime-state/ 与 .update.guard。
	if _, err := os.Stat(s.layout.UpdateStateFile()); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, outcomeNoTransaction, nil
		}
		return false, outcomeStoreError, err
	}
	// 状态文件存在时复用现有事务读取路径；该路径只会建立已存在目录内的
	// guard 协调 leaf，不会新增目录（runtime-state/ 此时已存在）。
	store, err := state.NewStore(ctx, s.layout)
	if err != nil {
		return false, outcomeStoreError, err
	}
	snapshot, err := store.ReadTransaction(ctx, state.TransactionUpdate)
	closeErr := store.Close()
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return false, outcomeNoTransaction, closeErr
		}
		return false, outcomeReadError, errors.Join(err, closeErr)
	}
	transaction := snapshot.State()
	if transaction.OperationID != operationID {
		return false, outcomeIdentityMismatch, closeErr
	}
	inspection := state.InspectTransaction(
		ctx,
		state.TransactionUpdate,
		transaction,
		state.NewSystemPIDProbe(),
		peerMutexProbe{},
	)
	if inspection.ProbeError != nil {
		return false, outcomeUnknown, errors.Join(inspection.ProbeError, closeErr)
	}
	switch inspection.Activity {
	case state.ActivityStale:
		return true, outcomeStale, closeErr
	case state.ActivityActive:
		return false, outcomeActive, closeErr
	default:
		return false, repoUpdateOutcome(inspection.Activity), closeErr
	}
}

func summarize(items []Item) Summary {
	summary := Summary{Total: len(items)}
	for _, item := range items {
		switch item.Status {
		case ItemCleaned:
			summary.Cleaned++
		case ItemSkipped:
			summary.Skipped++
		case ItemFailed:
			summary.Failed++
		}
	}
	return summary
}

func emitItemProgress(
	emitter *protocol.Emitter,
	id string,
	status protocol.ProgressStatus,
	message string,
) error {
	return emitter.EmitProgress(protocol.ProgressEvent{
		Stage:   protocol.StageCleanup,
		Status:  status,
		Message: id + "：" + message,
	})
}

func progressForStatus(status ItemStatus) protocol.ProgressStatus {
	switch status {
	case ItemCleaned:
		return protocol.ProgressSucceeded
	case ItemSkipped:
		return protocol.ProgressSkipped
	case ItemFailed:
		return protocol.ProgressFailed
	default:
		return protocol.ProgressRunning
	}
}

func operationErrorCode(err error, fallback protocol.Code) protocol.Code {
	var fileErr *filesystem.Error
	if errors.As(err, &fileErr) {
		return fileErr.Code()
	}
	return fallback
}
