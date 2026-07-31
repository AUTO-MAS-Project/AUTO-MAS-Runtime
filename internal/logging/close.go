package logging

import (
	"errors"
	"fmt"
)

// Close 按 writer、RuntimeLogFiles 的顺序幂等释放 Logger 自有资源。
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.closed {
		return l.closeErr
	}
	l.closed = true

	var writerErr error
	if !isNilInterface(l.writer) {
		if err := l.writer.Close(); err != nil {
			writerErr = fmt.Errorf("close active runtime log writer: %w", err)
		}
	}
	var filesErr error
	if !isNilInterface(l.files) {
		if err := l.files.Close(); err != nil {
			filesErr = fmt.Errorf("close runtime log files: %w", err)
		}
	}
	l.closeErr = errors.Join(writerErr, filesErr)
	return l.closeErr
}
