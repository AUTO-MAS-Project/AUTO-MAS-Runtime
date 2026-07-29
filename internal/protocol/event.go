package protocol

// Version 是当前 Runtime NDJSON 协议版本。
const Version = 1

// EventType 标识一种 NDJSON 事件。
type EventType string

const (
	TypeHello    EventType = "hello"
	TypeProgress EventType = "progress"
	TypeState    EventType = "state"
	TypeLog      EventType = "log"
	TypeWarning  EventType = "warning"
	TypeError    EventType = "error"
	TypeResult   EventType = "result"
)

// Common 包含所有协议事件共享的字段。
type Common struct {
	Protocol    int       `json:"protocol"`
	Type        EventType `json:"type"`
	OperationID string    `json:"operationId"`
	Sequence    uint64    `json:"sequence"`
	Timestamp   string    `json:"timestamp"`
}

// HelloEvent 描述 Runtime 及即将开始的操作。
type HelloEvent struct {
	Common
	RuntimeVersion string   `json:"runtimeVersion"`
	Command        string   `json:"command"`
	Capabilities   []string `json:"capabilities"`
}

// ProgressEvent 报告稳定 stage 的执行进度。
type ProgressEvent struct {
	Common
	Stage   Stage          `json:"stage"`
	Status  ProgressStatus `json:"status"`
	Current *int64         `json:"current,omitempty"`
	Total   *int64         `json:"total,omitempty"`
	Percent *float64       `json:"percent,omitempty"`
	Message string         `json:"message"`
}

// StateEvent 报告生命周期迁移或只读状态快照。
type StateEvent struct {
	Common
	Stage   Stage          `json:"stage"`
	Status  StateStatus    `json:"status"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// LogEvent 携带受管进程的一行可显示日志。
type LogEvent struct {
	Common
	Source  string `json:"source"`
	Stream  string `json:"stream"`
	Message string `json:"message"`
}

// WarningEvent 报告不会终止操作的异常情况。
type WarningEvent struct {
	Common
	Code        string         `json:"code"`
	Stage       Stage          `json:"stage"`
	Message     string         `json:"message"`
	Retryable   bool           `json:"retryable"`
	Remediation []string       `json:"remediation"`
	Details     map[string]any `json:"details"`
}

// MaxResultWarningSummaries 是终态 result 最多保留的 warning 快照数。
// warningCount 仍会统计全部已发射的 warning。
const MaxResultWarningSummaries = 256

// WarningSummary 是已发射 warning 与调用方可变输入隔离的业务字段快照，会随终态 result 返回。
type WarningSummary struct {
	Code        string         `json:"code"`
	Stage       Stage          `json:"stage"`
	Message     string         `json:"message"`
	Retryable   bool           `json:"retryable"`
	Remediation []string       `json:"remediation"`
	Details     map[string]any `json:"details"`
}

// ErrorEvent 报告操作的主错误。
type ErrorEvent struct {
	Common
	Code        string         `json:"code"`
	Stage       Stage          `json:"stage"`
	Message     string         `json:"message"`
	Retryable   bool           `json:"retryable"`
	Remediation []string       `json:"remediation"`
	Details     map[string]any `json:"details"`
}

// ResultEvent 表示顶层操作的最终结果。
type ResultEvent struct {
	Common
	Success     bool           `json:"success"`
	Code        string         `json:"code"`
	Stage       Stage          `json:"stage"`
	Status      string         `json:"status"`
	Message     string         `json:"message"`
	Retryable   bool           `json:"retryable"`
	Remediation []string       `json:"remediation"`
	Details     map[string]any `json:"details"`
}
