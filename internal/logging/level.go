package logging

// Level 表示 Runtime 本地日志级别。
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// String 返回稳定的日志级别字面量。
func (l Level) String() string {
	return string(l)
}

// Valid 报告日志级别是否属于冻结集合。
func (l Level) Valid() bool {
	switch l {
	case LevelDebug, LevelInfo, LevelWarn, LevelError:
		return true
	default:
		return false
	}
}
