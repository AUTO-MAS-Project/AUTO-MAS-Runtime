package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

type sequenceClock struct {
	values []time.Time
	index  int
}

func (c *sequenceClock) now() time.Time {
	if len(c.values) == 0 {
		return time.Time{}
	}
	index := c.index
	if index >= len(c.values) {
		index = len(c.values) - 1
	}
	c.index++
	return c.values[index]
}

func newWritableLogger(
	t *testing.T,
	clock func() time.Time,
) (*Logger, *fakeLogFiles, *fakeLogWriter, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	layout := mustTestLayout(t)
	now := clock()
	path, err := layout.RuntimeLogFile("doctor", now)
	if err != nil {
		t.Fatalf("RuntimeLogFile() error = %v", err)
	}
	fileOutput := &bytes.Buffer{}
	stderrOutput := &bytes.Buffer{}
	writer := &fakeLogWriter{
		path: path,
		write: func(p []byte) (int, error) {
			return fileOutput.Write(p)
		},
		close: func() error { return nil },
	}
	files := completeFakeFiles(writer)
	logger, err := newWithDependencies(
		t.Context(),
		layout,
		stderrOutput,
		"doctor",
		"01JTEST",
		func(context.Context, *config.Layout) (logFiles, error) { return files, nil },
		func() time.Time { return now },
		DefaultRetentionPolicy(),
	)
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}
	logger.clock = clock
	fileOutput.Reset()
	stderrOutput.Reset()
	cleanupConstructedLogger(t, logger)
	return logger, files, writer, fileOutput, stderrOutput
}

func decodeSingleLogLine(t *testing.T, output []byte) map[string]json.RawMessage {
	t.Helper()
	lines := bytes.Split(bytes.TrimSuffix(output, []byte{'\n'}), []byte{'\n'})
	if len(lines) != 1 {
		t.Fatalf("physical line count = %d, want 1", len(lines))
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(lines[0], &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	return decoded
}

func TestLogger_RecordWritesOnlyOperationJSONL(t *testing.T) {
	now := fixedEntryTime()
	logger, _, _, fileOutput, stderrOutput := newWritableLogger(t, func() time.Time { return now })
	result, err := logger.Record(
		t.Context(),
		LevelInfo,
		"operation\nmessage",
		map[string]any{"text": "甲\n乙"},
	)
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if !result.FileWritten || result.StderrWritten || result.Rotated {
		t.Fatalf("Record() result = %#v, want only file written", result)
	}
	decoded := decodeSingleLogLine(t, fileOutput.Bytes())
	if got := string(decoded["kind"]); got != `"operation"` {
		t.Fatalf("kind = %s, want operation", got)
	}
	if stderrOutput.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderrOutput.String())
	}
}

func TestLogger_DiagnosticWritesFileAndStderr(t *testing.T) {
	now := fixedEntryTime()
	logger, _, _, fileOutput, stderrOutput := newWritableLogger(t, func() time.Time { return now })
	result, err := logger.Diagnostic(
		t.Context(),
		LevelWarn,
		"diagnostic\nmessage",
		nil,
	)
	if err != nil {
		t.Fatalf("Diagnostic() error = %v", err)
	}
	if !result.FileWritten || !result.StderrWritten || result.Rotated {
		t.Fatalf("Diagnostic() result = %#v, want both sinks without rotation", result)
	}
	decoded := decodeSingleLogLine(t, fileOutput.Bytes())
	if got := string(decoded["kind"]); got != `"diagnostic"` {
		t.Fatalf("kind = %s, want diagnostic", got)
	}
	if got := bytes.Count(stderrOutput.Bytes(), []byte{'\n'}); got != 1 {
		t.Fatalf("stderr physical line count = %d, want 1", got)
	}
}

func TestLogger_DiagnosticReusesExactlyOneDetailsRawMessageAcrossSinks(t *testing.T) {
	tests := []struct {
		name       string
		fileErr    error
		stderrErr  error
		wantFile   bool
		wantStderr bool
	}{
		{name: "file succeeds", stderrErr: errors.New("stderr"), wantFile: true},
		{name: "stderr succeeds", fileErr: errors.New("file"), wantStderr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := fixedEntryTime()
			logger, _, writer, fileOutput, stderrOutput := newWritableLogger(t, func() time.Time { return now })
			writer.write = func(p []byte) (int, error) {
				if test.fileErr != nil {
					return 0, test.fileErr
				}
				return fileOutput.Write(p)
			}
			logger.stderr = entryWriterFunc(func(p []byte) (int, error) {
				if test.stderrErr != nil {
					return 0, test.stderrErr
				}
				return stderrOutput.Write(p)
			})
			value := &changingMarshaler{}
			result, err := logger.Diagnostic(
				t.Context(),
				LevelInfo,
				"message",
				map[string]any{"value": value},
			)
			if err == nil {
				t.Fatal("Diagnostic() error = nil, want injected sink error")
			}
			if value.calls != 1 {
				t.Fatalf("MarshalJSON calls = %d, want 1", value.calls)
			}
			if result.FileWritten != test.wantFile || result.StderrWritten != test.wantStderr {
				t.Fatalf("result = %#v, want file=%v stderr=%v", result, test.wantFile, test.wantStderr)
			}
			wantDetails := `{"value":{"call":1}}`
			if test.wantFile {
				if got := string(decodeSingleLogLine(t, fileOutput.Bytes())["details"]); got != wantDetails {
					t.Fatalf("file details = %s, want %s", got, wantDetails)
				}
			}
			if test.wantStderr && !strings.HasSuffix(
				strings.TrimSuffix(stderrOutput.String(), "\n"),
				" "+wantDetails,
			) {
				t.Fatalf("stderr = %q, want exact details suffix %q", stderrOutput.String(), wantDetails)
			}
		})
	}
}

func TestLogger_WriteResultReflectsEachSink(t *testing.T) {
	cause := errors.New("sink")
	tests := []struct {
		name       string
		fileN      int
		fileErr    error
		stderrN    int
		stderrErr  error
		wantFile   bool
		wantStderr bool
	}{
		{name: "both full", fileN: -1, stderrN: -1, wantFile: true, wantStderr: true},
		{name: "partial", fileN: 1, stderrN: 1, wantFile: true, wantStderr: true},
		{name: "full with errors", fileN: -1, fileErr: cause, stderrN: -1, stderrErr: cause, wantFile: true, wantStderr: true},
		{name: "zero", fileErr: cause, stderrErr: cause},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := fixedEntryTime()
			logger, _, writer, _, _ := newWritableLogger(t, func() time.Time { return now })
			writer.write = func(p []byte) (int, error) {
				if test.fileN < 0 {
					return len(p), test.fileErr
				}
				return test.fileN, test.fileErr
			}
			logger.stderr = entryWriterFunc(func(p []byte) (int, error) {
				if test.stderrN < 0 {
					return len(p), test.stderrErr
				}
				return test.stderrN, test.stderrErr
			})
			result, _ := logger.Diagnostic(t.Context(), LevelInfo, "message", nil)
			if result.FileWritten != test.wantFile || result.StderrWritten != test.wantStderr {
				t.Fatalf("result = %#v, want file=%v stderr=%v", result, test.wantFile, test.wantStderr)
			}
		})
	}
}

func TestLogger_WriteRejectsInvalidInputWithoutSideEffects(t *testing.T) {
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
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	tests := []struct {
		name    string
		ctx     context.Context
		level   Level
		details map[string]any
		want    error
	}{
		{name: "nil context", level: LevelInfo, want: ErrInvalidArgument},
		{name: "cancelled", ctx: cancelled, level: LevelInfo, want: context.Canceled},
		{name: "invalid level", ctx: t.Context(), level: Level("trace"), want: ErrInvalidLevel},
		{name: "invalid details", ctx: t.Context(), level: LevelInfo, details: map[string]any{"bad": make(chan int)}, want: ErrEncodeEntry},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := logger.Record(test.ctx, test.level, "message", test.details)
			if result != (WriteResult{}) || !errors.Is(err, test.want) {
				t.Fatalf("Record() = (%#v, %v), want zero/%v", result, err, test.want)
			}
		})
	}
	logger.clock = func() time.Time { return time.Time{} }
	result, err := logger.Record(t.Context(), LevelInfo, "zero clock", nil)
	if result != (WriteResult{}) || !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("Record(zero clock) = (%#v, %v), want zero/ErrInvalidTime", result, err)
	}
	if openCalls != 0 || listCalls != 0 || removeCalls != 0 || writeCalls != 0 {
		t.Fatalf("side effects = open %d/list %d/remove %d/write %d, want zero", openCalls, listCalls, removeCalls, writeCalls)
	}
}

func TestLogger_RotatesOnceAtLocalMidnightAndUsesClockLocation(t *testing.T) {
	layout := mustTestLayout(t)
	location := time.FixedZone("CST", 8*60*60)
	dayOne := time.Date(2026, 7, 29, 23, 59, 59, 0, location)
	dayTwo := time.Date(2026, 7, 30, 0, 0, 1, 0, location)
	oldPath, err := layout.RuntimeLogFile("doctor", dayOne)
	if err != nil {
		t.Fatalf("RuntimeLogFile(day one) error = %v", err)
	}
	newPath, err := layout.RuntimeLogFile("doctor", dayTwo)
	if err != nil {
		t.Fatalf("RuntimeLogFile(day two) error = %v", err)
	}
	oldWriter := fullFakeWriter(oldPath)
	newWriter := fullFakeWriter(newPath)
	clock := &sequenceClock{values: []time.Time{dayOne, dayTwo, dayTwo}}
	openCalls := 0
	files := &fakeLogFiles{
		openAppend: func(_ context.Context, _ string, date time.Time) (logWriter, error) {
			openCalls++
			if date.Format("20060102") == "20260729" {
				return oldWriter, nil
			}
			return newWriter, nil
		},
		list:   func(context.Context) ([]retainedFile, error) { return nil, nil },
		remove: func(context.Context, retainedFile) (removeResult, error) { return removeResult{}, nil },
		close:  func() error { return nil },
	}
	logger, err := newWithDependencies(
		t.Context(),
		layout,
		&bytes.Buffer{},
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
	first, err := logger.Record(t.Context(), LevelInfo, "first", nil)
	if err != nil {
		t.Fatalf("first Record() error = %v", err)
	}
	second, err := logger.Record(t.Context(), LevelInfo, "second", nil)
	if err != nil {
		t.Fatalf("second Record() error = %v", err)
	}
	if !first.Rotated || second.Rotated || openCalls != 2 {
		t.Fatalf("rotation results = %#v/%#v, open calls %d, want true/false and 2", first, second, openCalls)
	}
	if got := logger.LogPath(); got != newPath {
		t.Fatalf("LogPath() = %q, want %q", got, newPath)
	}
}

func TestLogger_RotationOpenFailureKeepsOldPathAndCanRetry(t *testing.T) {
	layout := mustTestLayout(t)
	dayOne := time.Date(2026, 7, 29, 23, 59, 59, 0, time.Local)
	dayTwo := dayOne.AddDate(0, 0, 1)
	oldPath, err := layout.RuntimeLogFile("doctor", dayOne)
	if err != nil {
		t.Fatalf("RuntimeLogFile(old) error = %v", err)
	}
	newPath, err := layout.RuntimeLogFile("doctor", dayTwo)
	if err != nil {
		t.Fatalf("RuntimeLogFile(new) error = %v", err)
	}
	var oldBytes bytes.Buffer
	oldWriter := &fakeLogWriter{
		path:  oldPath,
		write: func(p []byte) (int, error) { return oldBytes.Write(p) },
		close: func() error { return nil },
	}
	newWriter := fullFakeWriter(newPath)
	openErr := errors.New("open rotation")
	clock := &sequenceClock{values: []time.Time{dayOne, dayTwo, dayTwo}}
	openCalls := 0
	files := &fakeLogFiles{
		openAppend: func(context.Context, string, time.Time) (logWriter, error) {
			openCalls++
			switch openCalls {
			case 1:
				return oldWriter, nil
			case 2:
				return nil, openErr
			default:
				return newWriter, nil
			}
		},
		list:   func(context.Context) ([]retainedFile, error) { return nil, nil },
		remove: func(context.Context, retainedFile) (removeResult, error) { return removeResult{}, nil },
		close:  func() error { return nil },
	}
	var stderr bytes.Buffer
	logger, err := newWithDependencies(
		t.Context(),
		layout,
		&stderr,
		"doctor",
		"01JTEST",
		func(context.Context, *config.Layout) (logFiles, error) { return files, nil },
		clock.now,
		DefaultRetentionPolicy(),
	)
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}
	oldBytes.Reset()
	cleanupConstructedLogger(t, logger)
	first, err := logger.Diagnostic(t.Context(), LevelInfo, "first", nil)
	if !errors.Is(err, openErr) || first.FileWritten || !first.StderrWritten || first.Rotated {
		t.Fatalf("first Diagnostic() = (%#v, %v), want stderr-only open error", first, err)
	}
	if oldBytes.Len() != 0 || logger.LogPath() != oldPath {
		t.Fatalf("old bytes/path = %d/%q, want 0/%q", oldBytes.Len(), logger.LogPath(), oldPath)
	}
	second, err := logger.Record(t.Context(), LevelInfo, "retry", nil)
	if err != nil || !second.FileWritten || !second.Rotated || logger.LogPath() != newPath {
		t.Fatalf("retry Record() = (%#v, %v, path %q), want rotated new path", second, err, logger.LogPath())
	}
}

func TestLogger_RotationCloseFailureSwitchesAndReportsAppliedResult(t *testing.T) {
	layout := mustTestLayout(t)
	dayOne := time.Date(2026, 7, 29, 23, 59, 59, 0, time.Local)
	dayTwo := dayOne.AddDate(0, 0, 1)
	oldPath, _ := layout.RuntimeLogFile("doctor", dayOne)
	newPath, _ := layout.RuntimeLogFile("doctor", dayTwo)
	closeErr := errors.New("old writer close")
	oldWriter := fullFakeWriter(oldPath)
	oldWriter.close = func() error { return closeErr }
	var newBytes bytes.Buffer
	newWriter := &fakeLogWriter{
		path:  newPath,
		write: func(p []byte) (int, error) { return newBytes.Write(p) },
		close: func() error { return nil },
	}
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
		list:   func(context.Context) ([]retainedFile, error) { return nil, nil },
		remove: func(context.Context, retainedFile) (removeResult, error) { return removeResult{}, nil },
		close:  func() error { return nil },
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
	result, err := logger.Record(t.Context(), LevelInfo, "after midnight", nil)
	if !errors.Is(err, closeErr) || !result.Rotated || !result.FileWritten {
		t.Fatalf("Record() = (%#v, %v), want rotated/written with close error", result, err)
	}
	if logger.LogPath() != newPath || newBytes.Len() == 0 {
		t.Fatalf("new path/bytes = %q/%d, want %q/nonzero", logger.LogPath(), newBytes.Len(), newPath)
	}
}

type commitContextKey struct{}

var (
	errCommittedValueLost = errors.New("committed context lost value")
	errCommittedCancelled = errors.New("committed context remains cancellable")
)

func validateCommittedContext(ctx context.Context, want string) error {
	if got := ctx.Value(commitContextKey{}); got != want {
		return errCommittedValueLost
	}
	if ctx.Done() != nil || ctx.Err() != nil {
		return errCommittedCancelled
	}
	return nil
}

func TestLogger_CancelAfterCommitPreservesValueAndCompletesOpenListRemove(t *testing.T) {
	layout := mustTestLayout(t)
	dayOne := time.Date(2026, 7, 29, 23, 59, 59, 0, time.Local)
	dayTwo := dayOne.AddDate(0, 0, 1)
	oldPath, _ := layout.RuntimeLogFile("doctor", dayOne)
	newPath, _ := layout.RuntimeLogFile("doctor", dayTwo)
	retainedPath, _ := layout.RuntimeLogFile("workspace-sync", dayOne.AddDate(0, 0, -40))
	oldWriter := fullFakeWriter(oldPath)
	newWriter := fullFakeWriter(newPath)
	clock := &sequenceClock{values: []time.Time{dayOne, dayTwo}}
	openEntered := make(chan struct{})
	openRelease := make(chan struct{})
	openCalls := 0
	var seen []string
	files := &fakeLogFiles{
		openAppend: func(ctx context.Context, _ string, _ time.Time) (logWriter, error) {
			openCalls++
			if openCalls == 1 {
				return oldWriter, nil
			}
			close(openEntered)
			if err := waitForTestRelease(openRelease, "rotation OpenAppend release"); err != nil {
				return nil, err
			}
			if err := validateCommittedContext(ctx, "kept"); err != nil {
				return nil, err
			}
			seen = append(seen, "open")
			return newWriter, nil
		},
		list: func(ctx context.Context) ([]retainedFile, error) {
			if openCalls == 1 {
				return nil, nil
			}
			if err := validateCommittedContext(ctx, "kept"); err != nil {
				return nil, err
			}
			seen = append(seen, "list")
			return []retainedFile{
				fakeRetainedFile{name: filepath.Base(retainedPath), path: retainedPath},
			}, nil
		},
		remove: func(ctx context.Context, _ retainedFile) (removeResult, error) {
			if err := validateCommittedContext(ctx, "kept"); err != nil {
				return removeResult{}, err
			}
			seen = append(seen, "remove")
			return removeResult{mutationApplied: true}, nil
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
		RetentionPolicy{MaxAgeDays: 30, MaxFilesPerCommand: 30},
	)
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}
	cleanupConstructedLogger(t, logger)
	base := context.WithValue(t.Context(), commitContextKey{}, "kept")
	writeCtx, cancel := context.WithCancel(base)
	resultCh := make(chan struct {
		result WriteResult
		err    error
	}, 1)
	go func() {
		result, writeErr := logger.Record(writeCtx, LevelInfo, "rotate", nil)
		resultCh <- struct {
			result WriteResult
			err    error
		}{result: result, err: writeErr}
	}()
	waitForTestSignal(t, openEntered, "rotation OpenAppend entry")
	cancel()
	close(openRelease)
	got := receiveTestValue(t, resultCh, "committed rotation result")
	if got.err != nil {
		t.Fatalf("Record() error = %v", got.err)
	}
	if !got.result.Rotated || !got.result.FileWritten {
		t.Fatalf("Record() result = %#v, want rotated and written", got.result)
	}
	if want := []string{"open", "list", "remove"}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("committed calls = %#v, want %#v", seen, want)
	}
}
