package filesystem

import (
	"context"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// FileInspection 描述受管普通文件的只读存在性事实。
type FileInspection struct {
	Exists bool
}

// InspectManagedFile 校验文件路径链不含重解析点，并确认叶子是普通文件。
//
// 该能力只读且不会创建文件；不存在的末端返回 Exists=false。任何身份、
// 路径边界或文件类型无法证明的情况都返回错误，调用方必须失败关闭。
func InspectManagedFile(
	ctx context.Context,
	layout *config.Layout,
	path string,
) (FileInspection, error) {
	return inspectManagedFile(ctx, layout, path)
}
