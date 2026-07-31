package logging

import "errors"

var (
	// ErrInvalidArgument 表示调用参数不满足 logging 契约。
	ErrInvalidArgument = errors.New("logging argument is invalid")
	// ErrInvalidLevel 表示日志级别不在冻结集合内。
	ErrInvalidLevel = errors.New("logging level is invalid")
	// ErrInvalidRetention 表示保留策略字段不是正数。
	ErrInvalidRetention = errors.New("logging retention is invalid")
	// ErrInvalidTime 表示注入时钟返回零值。
	ErrInvalidTime = errors.New("logging clock returned invalid time")
	// ErrEncodeEntry 表示 details 或完整 entry 无法编码。
	ErrEncodeEntry = errors.New("encode log entry")
	// ErrClosed 表示 Logger 已经关闭。
	ErrClosed = errors.New("logger is closed")
)
