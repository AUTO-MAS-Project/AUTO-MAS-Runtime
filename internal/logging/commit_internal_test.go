package logging

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

type blockingMarshaler struct {
	entered chan struct{}
	release chan struct{}
}

func (m blockingMarshaler) MarshalJSON() ([]byte, error) {
	close(m.entered)
	if err := waitForTestRelease(m.release, "details encoding release"); err != nil {
		return nil, err
	}
	return []byte(`{"encoded":true}`), nil
}

func TestLogger_CancelBeforeCommitHasNoSideEffects(t *testing.T) {
	now := fixedEntryTime()
	logger, files, writer, _, _ := newWritableLogger(t, func() time.Time { return now })
	openCalls := 0
	listCalls := 0
	removeCalls := 0
	writeCalls := 0
	files.openAppend = func(context.Context, string, time.Time) (logWriter, error) {
		openCalls++
		return writer, nil
	}
	files.list = func(context.Context) ([]retainedFile, error) {
		listCalls++
		return nil, nil
	}
	files.remove = func(context.Context, retainedFile) (removeResult, error) {
		removeCalls++
		return removeResult{}, nil
	}
	writer.write = func(p []byte) (int, error) {
		writeCalls++
		return len(p), nil
	}
	stderrCalls := 0
	logger.stderr = entryWriterFunc(func(p []byte) (int, error) {
		stderrCalls++
		return len(p), nil
	})
	entered := make(chan struct{})
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	resultCh := make(chan struct {
		result WriteResult
		err    error
	}, 1)
	go func() {
		result, err := logger.Diagnostic(
			ctx,
			LevelInfo,
			"cancel before commit",
			map[string]any{"barrier": blockingMarshaler{entered: entered, release: release}},
		)
		resultCh <- struct {
			result WriteResult
			err    error
		}{result: result, err: err}
	}()
	waitForTestSignal(t, entered, "details encoding entry")
	cancel()
	close(release)
	got := receiveTestValue(t, resultCh, "pre-commit cancellation result")
	if got.result != (WriteResult{}) || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("Diagnostic() = (%#v, %v), want zero/context.Canceled", got.result, got.err)
	}
	if openCalls != 0 || listCalls != 0 || removeCalls != 0 || writeCalls != 0 || stderrCalls != 0 {
		t.Fatalf(
			"side effects = open %d/list %d/remove %d/file %d/stderr %d, want zero",
			openCalls, listCalls, removeCalls, writeCalls, stderrCalls,
		)
	}
}

func TestLogger_CancelAfterCommitCompletesBothSinks(t *testing.T) {
	now := fixedEntryTime()
	logger, _, writer, fileOutput, stderrOutput := newWritableLogger(t, func() time.Time { return now })
	entered := make(chan struct{})
	release := make(chan struct{})
	writer.write = func(p []byte) (int, error) {
		close(entered)
		if err := waitForTestRelease(release, "file sink release"); err != nil {
			return 0, err
		}
		return fileOutput.Write(p)
	}
	ctx, cancel := context.WithCancel(t.Context())
	resultCh := make(chan struct {
		result WriteResult
		err    error
	}, 1)
	go func() {
		result, err := logger.Diagnostic(ctx, LevelInfo, "committed", nil)
		resultCh <- struct {
			result WriteResult
			err    error
		}{result: result, err: err}
	}()
	waitForTestSignal(t, entered, "file sink entry")
	cancel()
	close(release)
	got := receiveTestValue(t, resultCh, "post-commit diagnostic result")
	if got.err != nil {
		t.Fatalf("Diagnostic() error = %v", got.err)
	}
	if !got.result.FileWritten || !got.result.StderrWritten {
		t.Fatalf("Diagnostic() result = %#v, want both sinks written", got.result)
	}
	if fileOutput.Len() == 0 || stderrOutput.Len() == 0 {
		t.Fatalf("sink bytes = file %d/stderr %d, want both nonzero", fileOutput.Len(), stderrOutput.Len())
	}
}

func TestLogger_CancelAfterRotationCommitPreservesCloseErrorAndResult(t *testing.T) {
	layout := mustTestLayout(t)
	dayOne := time.Date(2026, 7, 29, 23, 59, 59, 0, time.Local)
	dayTwo := dayOne.AddDate(0, 0, 1)
	oldPath, _ := layout.RuntimeLogFile("doctor", dayOne)
	newPath, _ := layout.RuntimeLogFile("doctor", dayTwo)
	closeEntered := make(chan struct{})
	closeRelease := make(chan struct{})
	closeErr := errors.New("old writer close")
	oldWriter := fullFakeWriter(oldPath)
	oldWriter.close = func() error {
		close(closeEntered)
		return errors.Join(
			closeErr,
			waitForTestRelease(closeRelease, "previous writer close release"),
		)
	}
	newWriter := fullFakeWriter(newPath)
	clock := &sequenceClock{values: []time.Time{dayOne, dayTwo}}
	openCalls := 0
	files := &fakeLogFiles{
		openAppend: func(context.Context, string, time.Time) (logWriter, error) {
			openCalls++
			if openCalls == 1 {
				return oldWriter, nil
			}
			return newWriter, nil
		},
		list: func(context.Context) ([]retainedFile, error) { return nil, nil },
		remove: func(context.Context, retainedFile) (removeResult, error) {
			return removeResult{}, nil
		},
		close: func() error { return nil },
	}
	logger, err := newWithDependencies(
		t.Context(),
		layout,
		io.Discard,
		"doctor",
		"01JTEST",
		func(context.Context, *config.Layout) (logFiles, error) { return files, nil },
		clock.now,
		DefaultRetentionPolicy(),
	)
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}
	cleanupConstructedLogger(t, logger)
	ctx, cancel := context.WithCancel(t.Context())
	resultCh := make(chan struct {
		result WriteResult
		err    error
	}, 1)
	go func() {
		result, writeErr := logger.Record(ctx, LevelInfo, "rotation committed", nil)
		resultCh <- struct {
			result WriteResult
			err    error
		}{result: result, err: writeErr}
	}()
	waitForTestSignal(t, closeEntered, "previous writer close entry")
	cancel()
	close(closeRelease)
	got := receiveTestValue(t, resultCh, "post-rotation cancellation result")
	if !errors.Is(got.err, closeErr) || errors.Is(got.err, context.Canceled) {
		t.Fatalf("Record() error = %v, want close error without context.Canceled", got.err)
	}
	if !got.result.Rotated || !got.result.FileWritten {
		t.Fatalf("Record() result = %#v, want rotated and file written", got.result)
	}
	if gotPath := logger.LogPath(); gotPath != newPath {
		t.Fatalf("LogPath() = %q, want %q", gotPath, newPath)
	}
}
