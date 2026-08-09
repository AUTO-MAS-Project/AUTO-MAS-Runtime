package filesystem

import (
	"context"
	"errors"
)

// ExternalPathInspection 是开发模式对 app-root 外路径的只读身份事实。
type ExternalPathInspection struct {
	Exists bool
}

var (
	// ErrExternalPathUnsafe 表示路径链包含无法安全证明的重解析点。
	ErrExternalPathUnsafe = errors.New("external path identity is unsafe")
	// ErrExternalPathNotOrdinary 表示叶子类型与调用方要求不符。
	ErrExternalPathNotOrdinary = errors.New("external path is not ordinary")
)

// InspectExternalPath 以句柄约束（Windows）或等价只读身份检查（其他平台）
// 验证路径链，绝不创建或改写目标。不存在的叶子返回 Exists=false。
func InspectExternalPath(ctx context.Context, path string, wantDirectory bool) (ExternalPathInspection, error) {
	return inspectExternalPath(ctx, path, wantDirectory)
}

// PathContains 以规范化身份判断 child 是否等于 parent 或位于其后代，
// 同时拒绝无法证明身份的重解析点与别名路径。
func PathContains(ctx context.Context, parent, child string) (bool, error) {
	return pathContains(ctx, parent, child)
}
