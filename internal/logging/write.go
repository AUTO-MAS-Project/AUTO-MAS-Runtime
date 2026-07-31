package logging

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Diagnostic 将 Runtime 诊断同步写入活动 JSONL 文件和 stderr。
func (l *Logger) Diagnostic(
	ctx context.Context,
	level Level,
	message string,
	details map[string]any,
) (WriteResult, error) {
	return l.write(ctx, entryDiagnostic, level, message, details)
}

// Record 将操作记录同步写入活动 JSONL 文件。
func (l *Logger) Record(
	ctx context.Context,
	level Level,
	message string,
	details map[string]any,
) (WriteResult, error) {
	return l.write(ctx, entryOperation, level, message, details)
}

func (l *Logger) write(
	ctx context.Context,
	kind entryKind,
	level Level,
	message string,
	details map[string]any,
) (WriteResult, error) {
	if ctx == nil {
		return WriteResult{}, ErrInvalidArgument
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return WriteResult{}, ErrClosed
	}

	now := l.clock()
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
		return WriteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, fmt.Errorf("commit log write: %w", err)
	}
	committedCtx := context.WithoutCancel(ctx)

	rotated, rotateErr := l.rotateLocked(committedCtx, now)
	result := WriteResult{Rotated: rotated}
	fileAvailable := rotateErr == nil || rotated

	var fileErr error
	if fileAvailable {
		result.FileWritten, fileErr = writeLine(l.writer, encoded.line)
		if fileErr != nil {
			fileErr = fmt.Errorf("write runtime log file: %w", fileErr)
		}
	}

	var stderrErr error
	if kind == entryDiagnostic {
		stderrLine := formatDiagnostic(
			now,
			level,
			l.command,
			l.operationID,
			message,
			encoded.detailsJSON,
		)
		result.StderrWritten, stderrErr = writeLine(l.stderr, stderrLine)
		if stderrErr != nil {
			stderrErr = fmt.Errorf("write runtime diagnostic stderr: %w", stderrErr)
		}
	}
	return result, errors.Join(rotateErr, fileErr, stderrErr)
}

func (l *Logger) rotateLocked(
	ctx context.Context,
	now time.Time,
) (bool, error) {
	date := now.Format("20060102")
	if date == l.activeDate {
		return false, nil
	}

	expectedPath, err := l.layout.RuntimeLogFile(l.command, now)
	if err != nil {
		return false, fmt.Errorf("derive rotated runtime log path: %w", err)
	}
	nextWriter, openErr := l.files.OpenAppend(ctx, l.command, now)
	if openErr != nil {
		return false, errors.Join(
			fmt.Errorf("open rotated runtime log: %w", openErr),
			closeWriterIfPresent(nextWriter),
		)
	}
	if isNilInterface(nextWriter) {
		return false, fmt.Errorf("open rotated runtime log: %w", ErrInvalidArgument)
	}
	if !sameRetentionPath(nextWriter.Path(), expectedPath) {
		return false, errors.Join(
			fmt.Errorf("validate rotated runtime log path: %w", ErrInvalidArgument),
			closeWriterIfPresent(nextWriter),
		)
	}

	previousWriter := l.writer
	l.writer = nextWriter
	l.activeDate = date
	l.activePath = nextWriter.Path()

	var previousCloseErr error
	if err := previousWriter.Close(); err != nil {
		previousCloseErr = fmt.Errorf("close previous runtime log writer: %w", err)
	}
	retentionErr := l.maintainRetentionLocked(ctx, now)
	if retentionErr != nil {
		retentionErr = fmt.Errorf("maintain retention after rotation: %w", retentionErr)
	}
	return true, errors.Join(previousCloseErr, retentionErr)
}

func closeWriterIfPresent(writer logWriter) error {
	if isNilInterface(writer) {
		return nil
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close unused runtime log writer: %w", err)
	}
	return nil
}
