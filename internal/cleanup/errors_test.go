package cleanup

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// TestMapLockError_CancelledMapsToOperationCancelled 证明锁错误映射优先识别
// 被包装的 context.Canceled，取消不会被误映射为 MUTEX_OPERATION_FAILED。
func TestMapLockError_CancelledMapsToOperationCancelled(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("acquire mutation: %w", context.Canceled)
	err := mapLockError(wrapped)
	var cleanupErr *Error
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("mapLockError() error type = %T, want *Error", err)
	}
	if cleanupErr.Code() != protocol.CodeOperationCancelled {
		t.Errorf("error code = %q, want OPERATION_CANCELLED", cleanupErr.Code())
	}
	if cleanupErr.Stage() != protocol.StageCleanup {
		t.Errorf("stage = %q, want cleanup", cleanupErr.Stage())
	}
	if !errors.Is(cleanupErr, context.Canceled) {
		t.Error("errors.Is(cleanupErr, context.Canceled) = false, want true")
	}
}

// TestMapOperationError_CancelledMapsToOperationCancelled 证明操作错误映射
// 优先识别被包装的 context.Canceled，取消不会被误映射为 GIT_REPO_CLEANUP_FAILED。
func TestMapOperationError_CancelledMapsToOperationCancelled(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("build plan: %w", context.Canceled)
	err := mapOperationError(
		wrapped,
		protocol.CodeGitRepoCleanupFailed,
		"无法生成清理计划",
	)
	var cleanupErr *Error
	if !errors.As(err, &cleanupErr) {
		t.Fatalf("mapOperationError() error type = %T, want *Error", err)
	}
	if cleanupErr.Code() != protocol.CodeOperationCancelled {
		t.Errorf("error code = %q, want OPERATION_CANCELLED", cleanupErr.Code())
	}
	if cleanupErr.Stage() != protocol.StageCleanup {
		t.Errorf("stage = %q, want cleanup", cleanupErr.Stage())
	}
	if !errors.Is(cleanupErr, context.Canceled) {
		t.Error("errors.Is(cleanupErr, context.Canceled) = false, want true")
	}
}
