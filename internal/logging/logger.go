package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

// Logger 将同一 Runtime 操作的诊断与操作记录写入受管本地 sink。
type Logger struct {
	// mu 保护活动日期、活动路径、writer、files、轮转、保留、两个 sink 写入、
	// closed 与 closeErr，使同一 Logger 的全部副作用处于单一线性化顺序。
	mu sync.Mutex

	layout      *config.Layout
	stderr      io.Writer
	command     string
	operationID string
	clock       func() time.Time
	retention   RetentionPolicy
	files       logFiles
	writer      logWriter
	activeDate  string
	activePath  string
	closed      bool
	closeErr    error
}

// New 创建并立即打开当前本地日期的 Runtime 日志文件。
func New(
	ctx context.Context,
	layout *config.Layout,
	stderr io.Writer,
	command string,
	operationID string,
	options ...Option,
) (*Logger, error) {
	values, err := applyOptions(options...)
	if err != nil {
		return nil, err
	}
	return newWithDependencies(
		ctx,
		layout,
		stderr,
		command,
		operationID,
		newFilesystemLogFiles,
		values.clock,
		values.retention,
	)
}

func newWithDependencies(
	ctx context.Context,
	layout *config.Layout,
	stderr io.Writer,
	command string,
	operationID string,
	filesFactory logFilesFactory,
	clock func() time.Time,
	retention RetentionPolicy,
) (*Logger, error) {
	now, expectedPath, err := validateNewArguments(
		ctx,
		layout,
		stderr,
		command,
		operationID,
		filesFactory,
		clock,
		retention,
	)
	if err != nil {
		return nil, err
	}

	files, factoryErr := filesFactory(ctx, layout)
	if factoryErr != nil {
		closeErr := closeFilesIfPresent(files)
		return nil, errors.Join(
			fmt.Errorf("create runtime log files: %w", factoryErr),
			closeErr,
		)
	}
	if isNilInterface(files) {
		return nil, fmt.Errorf("create runtime log files: %w", ErrInvalidArgument)
	}

	writer, openErr := files.OpenAppend(ctx, command, now)
	if openErr != nil {
		return nil, errors.Join(
			fmt.Errorf("open current runtime log: %w", openErr),
			closeOwned(writer, files),
		)
	}
	if isNilInterface(writer) {
		return nil, errors.Join(
			fmt.Errorf("open current runtime log: %w", ErrInvalidArgument),
			closeOwned(writer, files),
		)
	}
	if !filepath.IsAbs(writer.Path()) || !sameRetentionPath(writer.Path(), expectedPath) {
		return nil, errors.Join(
			fmt.Errorf("validate current runtime log path: %w", ErrInvalidArgument),
			closeOwned(writer, files),
		)
	}

	logger := &Logger{
		layout:      layout,
		stderr:      stderr,
		command:     command,
		operationID: operationID,
		clock:       clock,
		retention:   retention,
		files:       files,
		writer:      writer,
		activeDate:  now.Format("20060102"),
		activePath:  writer.Path(),
	}
	if err := logger.maintainRetentionLocked(ctx, now); err != nil {
		return nil, errors.Join(
			fmt.Errorf("maintain runtime log retention: %w", err),
			closeOwned(logger.writer, logger.files),
		)
	}
	return logger, nil
}

// LogPath 返回最后一个成功打开的活动日志绝对路径。
func (l *Logger) LogPath() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.activePath
}

func validateNewArguments(
	ctx context.Context,
	layout *config.Layout,
	stderr io.Writer,
	command string,
	operationID string,
	filesFactory logFilesFactory,
	clock func() time.Time,
	retention RetentionPolicy,
) (time.Time, string, error) {
	if ctx == nil || layout == nil || isNilInterface(stderr) {
		return time.Time{}, "", ErrInvalidArgument
	}
	if command == "" || operationID == "" ||
		strings.ContainsRune(command, '\x00') ||
		strings.ContainsRune(operationID, '\x00') {
		return time.Time{}, "", ErrInvalidArgument
	}
	if filesFactory == nil || clock == nil {
		return time.Time{}, "", ErrInvalidArgument
	}
	if err := validateRetention(retention); err != nil {
		return time.Time{}, "", err
	}
	now := clock()
	if now.IsZero() {
		return time.Time{}, "", ErrInvalidTime
	}
	expectedPath, err := layout.RuntimeLogFile(command, now)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("validate logging command: %w: %w", ErrInvalidArgument, err)
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, "", fmt.Errorf("construct logger: %w", err)
	}
	return now, expectedPath, nil
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func closeFilesIfPresent(files logFiles) error {
	if isNilInterface(files) {
		return nil
	}
	if err := files.Close(); err != nil {
		return fmt.Errorf("close runtime log files: %w", err)
	}
	return nil
}

func closeOwned(writer logWriter, files logFiles) error {
	var writerErr error
	if !isNilInterface(writer) {
		if err := writer.Close(); err != nil {
			writerErr = fmt.Errorf("close runtime log writer: %w", err)
		}
	}
	return errors.Join(writerErr, closeFilesIfPresent(files))
}

func retentionContextError(
	ctx context.Context,
	operationErr error,
) error {
	contextErr := ctx.Err()
	if contextErr == nil {
		switch {
		case errors.Is(operationErr, context.Canceled):
			contextErr = context.Canceled
		case errors.Is(operationErr, context.DeadlineExceeded):
			contextErr = context.DeadlineExceeded
		}
	}
	if contextErr == nil {
		return nil
	}
	return errors.Join(operationErr, contextErr)
}

func (l *Logger) maintainRetentionLocked(ctx context.Context, now time.Time) error {
	listed, listErr := l.files.List(ctx)
	var listFailure error
	if listErr != nil {
		listFailure = fmt.Errorf("list retained runtime logs: %w", listErr)
	}
	if cancellationErr := retentionContextError(ctx, listFailure); cancellationErr != nil {
		return cancellationErr
	}

	candidates := make([]retentionCandidate, 0, len(listed))
	for _, file := range listed {
		if candidate, ok := parseRetainedFile(l.layout, file, now.Location()); ok {
			candidates = append(candidates, candidate)
		}
	}

	removals := selectRetentionRemovals(
		candidates,
		l.activePath,
		now,
		l.retention,
	)
	removed := 0
	failures := make([]error, 0, len(removals)+1)
	if listFailure != nil {
		failures = append(failures, listFailure)
	}
	for _, file := range removals {
		if cancellationErr := retentionContextError(ctx, nil); cancellationErr != nil {
			return errors.Join(errors.Join(failures...), cancellationErr)
		}

		result, removeErr := l.files.Remove(ctx, file)
		if result.mutationApplied {
			removed++
		}
		var removeFailure error
		if removeErr != nil {
			removeFailure = fmt.Errorf(
				"remove retained runtime log %s: %w",
				file.Name(),
				removeErr,
			)
		}
		if cancellationErr := retentionContextError(
			ctx,
			removeFailure,
		); cancellationErr != nil {
			return errors.Join(errors.Join(failures...), cancellationErr)
		}
		if removeFailure != nil {
			failures = append(failures, removeFailure)
		}
	}

	details := map[string]any{
		"scanned":            len(listed),
		"removed":            removed,
		"failed":             len(failures),
		"maxAgeDays":         l.retention.MaxAgeDays,
		"maxFilesPerCommand": l.retention.MaxFilesPerCommand,
	}
	if len(failures) == 0 {
		if cancellationErr := retentionContextError(ctx, nil); cancellationErr != nil {
			return cancellationErr
		}
		return l.writeMaintenanceLocked(
			now,
			LevelInfo,
			"日志保留清理完成",
			details,
		)
	}

	messages := make([]string, 0, len(failures))
	for _, failure := range failures {
		messages = append(messages, failure.Error())
	}
	details["errors"] = messages
	if cancellationErr := retentionContextError(ctx, nil); cancellationErr != nil {
		return errors.Join(errors.Join(failures...), cancellationErr)
	}
	warningErr := l.writeMaintenanceLocked(
		now,
		LevelWarn,
		"日志保留清理存在失败",
		details,
	)
	if warningErr == nil {
		return nil
	}
	return errors.Join(errors.Join(failures...), warningErr)
}

func (l *Logger) writeMaintenanceLocked(
	now time.Time,
	level Level,
	message string,
	details map[string]any,
) error {
	kind := entryOperation
	if level == LevelWarn {
		kind = entryDiagnostic
	}
	encoded, err := encodeEntry(
		now,
		level,
		kind,
		l.command,
		l.operationID,
		message,
		details,
	)
	if err != nil {
		return err
	}
	if level != LevelWarn {
		_, err := writeLine(l.writer, encoded.line)
		if err != nil {
			return fmt.Errorf("write retention record: %w", err)
		}
		return nil
	}
	stderrLine := formatDiagnostic(
		now,
		level,
		l.command,
		l.operationID,
		message,
		encoded.detailsJSON,
	)
	_, err = writeDiagnosticSinks(l.writer, l.stderr, encoded.line, stderrLine)
	return err
}
