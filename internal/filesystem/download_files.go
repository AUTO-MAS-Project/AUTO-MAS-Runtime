package filesystem

// DownloadFiles 表示受管下载目录能力。
type DownloadFiles struct{}

// DownloadSession 表示绑定到单个临时文件句柄的下载会话。
type DownloadSession struct{}

// PublishResult 报告 no-replace 发布是否已经生效。
type PublishResult struct {
	Published bool
}

// AbortResult 报告下载临时文件是否已经移除。
type AbortResult struct {
	Removed bool
}
