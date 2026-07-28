package protocol

// Version is the current Runtime NDJSON protocol version.
const Version = 1

// EventType identifies an NDJSON event.
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

// Common contains the fields shared by every protocol event.
type Common struct {
	Protocol    int       `json:"protocol"`
	Type        EventType `json:"type"`
	OperationID string    `json:"operationId"`
	Sequence    uint64    `json:"sequence"`
	Timestamp   string    `json:"timestamp"`
}

// HelloEvent describes the Runtime and the operation being started.
type HelloEvent struct {
	Common
	RuntimeVersion string   `json:"runtimeVersion"`
	Command        string   `json:"command"`
	Capabilities   []string `json:"capabilities"`
}

// ProgressEvent reports progress for a stable stage.
type ProgressEvent struct {
	Common
	Stage   string   `json:"stage"`
	Status  string   `json:"status"`
	Current *int64   `json:"current,omitempty"`
	Total   *int64   `json:"total,omitempty"`
	Percent *float64 `json:"percent,omitempty"`
	Message string   `json:"message"`
}

// StateEvent reports a product lifecycle state transition.
type StateEvent struct {
	Common
	Stage   string         `json:"stage"`
	Status  string         `json:"status"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// LogEvent carries one displayable line from a managed process.
type LogEvent struct {
	Common
	Source  string `json:"source"`
	Stream  string `json:"stream"`
	Message string `json:"message"`
}

// WarningEvent reports a condition that does not stop the operation.
type WarningEvent struct {
	Common
	Code        string         `json:"code"`
	Stage       string         `json:"stage"`
	Message     string         `json:"message"`
	Retryable   bool           `json:"retryable"`
	Remediation []string       `json:"remediation"`
	Details     map[string]any `json:"details"`
}

// ErrorEvent reports the primary failure of an operation.
type ErrorEvent struct {
	Common
	Code        string         `json:"code"`
	Stage       string         `json:"stage"`
	Message     string         `json:"message"`
	Retryable   bool           `json:"retryable"`
	Remediation []string       `json:"remediation"`
	Details     map[string]any `json:"details"`
}

// ResultEvent is the final outcome of a top-level operation.
type ResultEvent struct {
	Common
	Success     bool           `json:"success"`
	Code        string         `json:"code"`
	Stage       string         `json:"stage"`
	Status      string         `json:"status"`
	Message     string         `json:"message"`
	Retryable   bool           `json:"retryable"`
	Remediation []string       `json:"remediation"`
	Details     map[string]any `json:"details"`
}
