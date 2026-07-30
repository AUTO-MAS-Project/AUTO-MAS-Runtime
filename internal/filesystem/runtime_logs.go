package filesystem

import (
	"sync"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

type runtimeLogOwner struct {
	marker byte
}

type runtimeLogDependencies struct {
	api pathAPI
}

// RuntimeLogFiles 表示受管 Runtime 日志目录能力。
type RuntimeLogFiles struct {
	mu sync.Mutex // 保护 closed、closeErr、pins 以及 open/list/remove/close 的线性化。

	layout   *config.Layout
	api      pathAPI
	owner    *runtimeLogOwner
	pins     [3]pinnedObject
	closed   bool
	closeErr error
}

// RuntimeLogWriter 表示绑定到已验证日志文件的写入能力。
type RuntimeLogWriter struct {
	mu sync.Mutex // 保护 file、pins、closed 和 closeErr。

	api      pathAPI
	path     string
	file     pinnedObject
	pins     [3]pinnedObject
	closed   bool
	closeErr error
}

// RuntimeLogFile 表示只能由所属目录能力消费的日志文件令牌。
type RuntimeLogFile struct {
	owner        *runtimeLogOwner
	path         string
	name         string
	volumeSerial uint64
	fileID       [16]byte
}

// RemoveResult 报告日志删除副作用是否已经发生。
type RemoveResult struct {
	MutationApplied bool
}

// Path 返回仅供诊断的日志文件绝对路径。
func (f RuntimeLogFile) Path() string {
	return f.path
}

// Name 返回仅供诊断的日志文件名。
func (f RuntimeLogFile) Name() string {
	return f.name
}
