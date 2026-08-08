package uv

import (
	"context"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

// TreeRemover 是需要删除受管目录的服务所消费的最小能力。
type TreeRemover interface {
	RemoveTree(context.Context, filesystem.DeleteRequest) (filesystem.DeleteResult, error)
}
