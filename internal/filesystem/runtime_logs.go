package filesystem

// RuntimeLogFiles 表示受管 Runtime 日志目录能力。
type RuntimeLogFiles struct{}

// RuntimeLogWriter 表示绑定到已验证日志文件的写入能力。
type RuntimeLogWriter struct{}

// RuntimeLogFile 表示只能由所属目录能力消费的日志文件令牌。
type RuntimeLogFile struct{}

// RemoveResult 报告日志删除副作用是否已经发生。
type RemoveResult struct {
	MutationApplied bool
}
