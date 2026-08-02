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

// planItem 是清理计划中的单条目标。
type planItem struct {
	id          string
	kind        filesystem.DeleteKind
	target      string
	operationID string
	reason      string
	preStatus   ItemStatus
	preMessage  string
	preDetails  map[string]any
}

// Run 获取 mutation 锁后执行清理计划。
// 任一条目失败时命令返回 GIT_REPO_CLEANUP_FAILED，details 保留完整副作用报告。
func (s *Service) Run(ctx context.Context, emitter *protocol.Emitter) (report Report, err error) {
	if err := ctx.Err(); err != nil {
		return Report{}, NewError(
			protocol.CodeOperationCancelled,
			protocol.StageCleanup,
			"清理已取消",
			map[string]any{},
			err,
		)
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
		return Report{}, NewError(
			protocol.CodeOperationCancelled,
			protocol.StageCleanup,
			"清理已取消",
			map[string]any{},
			err,
		)
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
			return report, NewError(
				protocol.CodeOperationCancelled,
				protocol.StageCleanup,
				"清理已取消",
				ReportDetails(report),
				err,
			)
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

func (s *Service) buildPlan(ctx context.Context, cleanupOperationID string) ([]planItem, error) {
	items := []planItem{
		{
			id:          "uv-cache",
			kind:        filesystem.DeleteUVCache,
			target:      s.layout.UVCacheDir(),
			operationID: cleanupOperationID,
			reason:      "cleanup",
		},
		{
			id:          "download-temp",
			kind:        filesystem.DeleteDownloadTemporary,
			target:      s.layout.DownloadCacheDir(),
			operationID: cleanupOperationID,
			reason:      "cleanup",
		},
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
	if entry.preStatus != "" {
		if err := emitItemProgress(
			emitter,
			entry.id,
			progressForStatus(entry.preStatus),
			entry.preMessage,
		); err != nil {
			return Item{}, err
		}
		return Item{
			ID:      entry.id,
			Status:  entry.preStatus,
			Message: entry.preMessage,
			Details: entry.preDetails,
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
		Kind:        entry.kind,
		Target:      entry.target,
		OperationID: entry.operationID,
		Reason:      entry.reason,
	})
	if err != nil {
		code := operationErrorCode(err, protocol.CodeGitRepoCleanupFailed)
		item := Item{
			ID:      entry.id,
			Status:  ItemFailed,
			Message: safeFailureMessage(code),
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

// enumeratePythonCaches 在 repo 下寻找 __pycache__ 目录。
// 候选目录先经 filesystem.Canonicalize 验证普通目录身份，Junction/符号链接
// 一律不删除并记为失败；遍历不跟随任何重解析点。
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
	err := filepath.WalkDir(repo, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() && entry.Name() == "__pycache__" {
			counter++
			id := "python-cache-" + strconv.Itoa(counter)
			if _, err := filesystem.Canonicalize(path); err != nil {
				items = append(items, planItem{
					id:         id,
					preStatus:  ItemFailed,
					preMessage: "目录身份不明确",
					preDetails: map[string]any{"code": string(protocol.CodeGitRepoCleanupFailed)},
				})
				return fs.SkipDir
			}
			items = append(items, planItem{
				id:          id,
				kind:        filesystem.DeletePythonCache,
				target:      path,
				operationID: cleanupOperationID,
				reason:      "cleanup",
			})
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
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "repo.update-") {
			continue
		}
		operationID := strings.TrimPrefix(entry.Name(), "repo.update-")
		expected, err := s.layout.RepoUpdateDir(operationID)
		if err != nil || filepath.Base(expected) != entry.Name() {
			items = append(items, planItem{
				id:         "repo-update-invalid",
				preStatus:  ItemFailed,
				preMessage: "目录身份不明确",
				preDetails: map[string]any{"code": string(protocol.CodeGitRepoCleanupFailed)},
			})
			continue
		}
		id := "repo-update-" + operationID
		cleanable, outcome, err := s.classifyRepoUpdate(ctx, operationID)
		switch {
		case err != nil:
			items = append(items, planItem{
				id:         id,
				preStatus:  ItemFailed,
				preMessage: "状态校验失败",
				preDetails: map[string]any{"code": string(protocol.CodeGitRepoCleanupFailed)},
			})
		case cleanable:
			items = append(items, planItem{
				id:          id,
				kind:        filesystem.DeleteRepositoryUpdate,
				target:      expected,
				operationID: operationID,
				reason:      "cleanup",
			})
		case outcome == "active":
			items = append(items, planItem{
				id:         id,
				preStatus:  ItemSkipped,
				preMessage: "更新仍在进行",
			})
		default:
			message := "状态身份不匹配"
			switch outcome {
			case "no-transaction":
				message = "缺少更新事务"
			case "identity-mismatch":
				message = "目录身份与事务不匹配"
			case "inconsistent":
				message = "事务 PID 仍存活"
			}
			items = append(items, planItem{
				id:         id,
				preStatus:  ItemFailed,
				preMessage: message,
				preDetails: map[string]any{"code": string(protocol.CodeGitRepoCleanupFailed)},
			})
		}
	}
	return items, nil
}

// classifyRepoUpdate 读取 update 事务并判定是否可安全清理。
// cleanup 已持有 mutation 租约，任何 peer 都不可能持有同一 Mutex，
// 因此对端探测恒为 free，活动性由 PID 探针区分 stale/inconsistent。
func (s *Service) classifyRepoUpdate(ctx context.Context, operationID string) (bool, string, error) {
	store, err := state.NewStore(ctx, s.layout)
	if err != nil {
		return false, "store-error", err
	}
	snapshot, err := store.ReadTransaction(ctx, state.TransactionUpdate)
	closeErr := store.Close()
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return false, "no-transaction", closeErr
		}
		return false, "read-error", errors.Join(err, closeErr)
	}
	transaction := snapshot.State()
	if transaction.OperationID != operationID {
		return false, "identity-mismatch", closeErr
	}
	inspection := state.InspectTransaction(
		ctx,
		state.TransactionUpdate,
		transaction,
		state.NewSystemPIDProbe(),
		peerMutexProbe{},
	)
	if inspection.ProbeError != nil {
		return false, "unknown", errors.Join(inspection.ProbeError, closeErr)
	}
	switch inspection.Activity {
	case state.ActivityStale:
		return true, "stale", closeErr
	case state.ActivityActive:
		return false, "active", closeErr
	default:
		return false, string(inspection.Activity), closeErr
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

func safeFailureMessage(code protocol.Code) string {
	switch code {
	case protocol.CodePathOutsideManagedRoot:
		return "目标超出受管根"
	case protocol.CodeUnsafeReparsePoint:
		return "目标为不安全的链接"
	case protocol.CodeDirectoryOccupied:
		return "目录被占用"
	default:
		return "删除失败"
	}
}
