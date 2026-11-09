package logging

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

// appendFileLocked 只重试整条未写入的容量拒绝，真正 I/O 故障保留原错误链。
// 调用方持有 Logger.mu；维护记录也走此入口，分卷本身不递归写维护记录。
func (l *Logger) appendFileLocked(ctx context.Context, now time.Time, line []byte) (bool, bool, error) {
	if int64(len(line)) > filesystem.MaxRuntimeLogBytes {
		return false, false, fmt.Errorf("runtime log entry exceeds file capacity: %w", filesystem.ErrRuntimeLogCapacity)
	}
	written, err := writeLine(l.writer, line)
	if written || !errors.Is(err, filesystem.ErrRuntimeLogCapacity) {
		return written, false, err
	}
	next, openErr := l.files.OpenNext(ctx, l.command, now)
	if openErr != nil {
		return false, false, errors.Join(fmt.Errorf("create runtime log segment: %w", openErr), closeWriterIfPresent(next))
	}
	if isNilInterface(next) {
		return false, false, fmt.Errorf("create runtime log segment: %w", ErrInvalidArgument)
	}
	previousName, _ := parseRetainedPath(l.layout, filepath.Base(l.activePath), l.activePath, now.Location())
	nextName, _ := parseRetainedPath(l.layout, filepath.Base(next.Path()), next.Path(), now.Location())
	if !matchesLogPath(l.layout, l.command, now, next.Path()) || nextName.sequence <= previousName.sequence {
		return false, false, errors.Join(fmt.Errorf("validate runtime log segment: %w", ErrInvalidArgument), closeWriterIfPresent(next))
	}
	previous := l.writer
	l.writer = next
	l.activeDate = now.Format("20060102")
	l.activePath = next.Path()
	var closeErr error
	if err := previous.Close(); err != nil {
		closeErr = fmt.Errorf("close previous runtime log segment: %w", err)
	}
	written, err = writeLine(next, line)
	return written, true, errors.Join(closeErr, err)
}
