//go:build !windows

package backend

import (
	"context"
	"io"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

// NewProductionManagedSupervisor 在非 Windows 平台失败关闭；首版不支持跨平台进程监督。
func NewProductionManagedSupervisor(
	context.Context,
	*config.Layout,
	io.Writer,
	func() time.Time,
) (*ManagedSupervisor, error) {
	return nil, newError(protocol.CodeUnsupportedMode, protocol.StageBackendSpawn, "受管后端监督不支持当前平台", map[string]any{"reason": "platform_unsupported"}, nil)
}
