package filesystem

import (
	"sync"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// DownloadFiles 表示受管下载目录能力。
type DownloadFiles struct {
	layout *config.Layout
	api    pathAPI
}

type downloadFileDependencies struct {
	api pathAPI
}

type downloadState uint8

const (
	downloadOpen downloadState = iota
	downloadPublished
	downloadAborted
)

// DownloadSession 表示绑定到单个临时文件句柄的下载会话。
type DownloadSession struct {
	mu sync.Mutex // 保护 part、pins、state、结果和 closeErr。

	api       pathAPI
	path      string
	partPath  string
	part      pinnedObject
	pins      [4]pinnedObject
	state     downloadState
	published PublishResult
	abort     AbortResult
	closeErr  error
}

// PublishResult 报告 no-replace 发布是否已经生效。
type PublishResult struct {
	Published bool
}

// AbortResult 报告下载临时文件是否已经移除。
type AbortResult struct {
	Removed bool
}
