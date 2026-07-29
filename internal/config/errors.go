package config

import "errors"

var (
	// ErrEmptyPath 表示必需路径为空。
	ErrEmptyPath = errors.New("path is empty")
	// ErrPathContainsNUL 表示路径包含 NUL。
	ErrPathContainsNUL = errors.New("path contains nul")
	// ErrBaseNotAbsolute 表示显式 base 不是绝对路径。
	ErrBaseNotAbsolute = errors.New("base path is not absolute")
	// ErrInvalidPath 表示路径形式无法安全解析。
	ErrInvalidPath = errors.New("path is invalid")
	// ErrAppRootIsRoot 表示 app root 指向卷根。
	ErrAppRootIsRoot = errors.New("app root is a volume root")
	// ErrInvalidSegment 表示动态文件名 segment 非法。
	ErrInvalidSegment = errors.New("path segment is invalid")
	// ErrInvalidLogDate 表示日志日期为零值。
	ErrInvalidLogDate = errors.New("log date is invalid")
)
