package logging

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

type closeSpy struct {
	bytes.Buffer
	closeCalls int
}

func (s *closeSpy) Close() error {
	s.closeCalls++
	return nil
}

func newCloseTestLogger(
	t *testing.T,
	stderr *closeSpy,
) (*Logger, *fakeLogWriter, *fakeLogFiles, *bytes.Buffer) {
	t.Helper()
	layout := mustTestLayout(t)
	now := fixedEntryTime()
	path, err := layout.RuntimeLogFile("doctor", now)
	if err != nil {
		t.Fatalf("RuntimeLogFile() error = %v", err)
	}
	fileOutput := &bytes.Buffer{}
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
		stderr,
		"doctor",
		"01JTEST",
		func(context.Context, *config.Layout) (logFiles, error) { return files, nil },
		func() time.Time { return now },
		DefaultRetentionPolicy(),
	)
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}
	fileOutput.Reset()
	t.Cleanup(func() {
		logger.mu.Lock()
		wasOpen := !logger.closed
		logger.mu.Unlock()
		if err := logger.Close(); err != nil && wasOpen {
			t.Errorf("cleanup Close() error = %v", err)
		}
	})
	return logger, writer, files, fileOutput
}

func TestLogger_CloseIsIdempotentAndCachesJoinedError(t *testing.T) {
	logger, writer, files, _ := newCloseTestLogger(t, &closeSpy{})
	writerErr := errors.New("writer close")
	filesErr := errors.New("files close")
	writerCalls := 0
	filesCalls := 0
	writer.close = func() error {
		writerCalls++
		return writerErr
	}
	files.close = func() error {
		filesCalls++
		return filesErr
	}

	first := logger.Close()
	second := logger.Close()
	if !errors.Is(first, writerErr) || !errors.Is(first, filesErr) {
		t.Fatalf("first Close() error = %v, want both causes", first)
	}
	if !errors.Is(second, writerErr) || !errors.Is(second, filesErr) {
		t.Fatalf("second Close() error = %v, want cached causes", second)
	}
	if first != second {
		t.Fatalf("Close() returned different cached error values: %p vs %p", first, second)
	}
	if writerCalls != 1 || filesCalls != 1 {
		t.Fatalf("close calls = writer %d/files %d, want 1/1", writerCalls, filesCalls)
	}
}

func TestLogger_CloseDoesNotCloseStderr(t *testing.T) {
	stderr := &closeSpy{}
	logger, _, _, _ := newCloseTestLogger(t, stderr)
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if stderr.closeCalls != 0 {
		t.Fatalf("stderr Close calls = %d, want 0", stderr.closeCalls)
	}
}

func TestLogger_WriteAfterCloseReturnsErrClosed(t *testing.T) {
	logger, writer, _, _ := newCloseTestLogger(t, &closeSpy{})
	writeCalls := 0
	writer.write = func(p []byte) (int, error) {
		writeCalls++
		return len(p), nil
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	result, err := logger.Record(t.Context(), LevelInfo, "after close", nil)
	if result != (WriteResult{}) || !errors.Is(err, ErrClosed) {
		t.Fatalf("Record(after Close) = (%#v, %v), want zero/ErrClosed", result, err)
	}
	if writeCalls != 0 {
		t.Fatalf("write calls after Close = %d, want 0", writeCalls)
	}
}

func TestLogger_LogPathRemainsAfterClose(t *testing.T) {
	logger, _, _, _ := newCloseTestLogger(t, &closeSpy{})
	want := logger.LogPath()
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := logger.LogPath(); got != want {
		t.Fatalf("LogPath(after Close) = %q, want %q", got, want)
	}
}

func TestLogger_CloseAfterStartedWritePreservesRecordAndRejectsLaterWrite(t *testing.T) {
	logger, writer, files, fileOutput := newCloseTestLogger(t, &closeSpy{})
	events := &testEventLedger{}
	writeEntered := make(chan struct{})
	writeRelease := make(chan struct{})
	var writeReleaseOnce sync.Once
	releaseWrite := func() {
		writeReleaseOnce.Do(func() { close(writeRelease) })
	}
	t.Cleanup(releaseWrite)
	writer.write = func(p []byte) (int, error) {
		events.add("file-write-enter")
		close(writeEntered)
		if err := waitForTestRelease(writeRelease, "started write release"); err != nil {
			return 0, err
		}
		n, err := fileOutput.Write(p)
		events.add("file-write-exit")
		return n, err
	}
	writerCloseCalls := 0
	filesCloseCalls := 0
	writer.close = func() error {
		writerCloseCalls++
		events.add("writer-close")
		return nil
	}
	files.close = func() error {
		filesCloseCalls++
		events.add("files-close")
		return nil
	}

	writeDone := make(chan error, 1)
	go func() {
		_, err := logger.Record(t.Context(), LevelInfo, "started", nil)
		writeDone <- err
	}()
	waitForTestSignal(t, writeEntered, "started write entry")
	if logger.mu.TryLock() {
		logger.mu.Unlock()
		releaseWrite()
		t.Fatal("Logger mutex is not held during started write")
	}
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- logger.Close()
	}()
	events.add("write-release")
	releaseWrite()
	if err := receiveTestValue(t, writeDone, "started write result"); err != nil {
		t.Fatalf("started Record() error = %v", err)
	}
	if err := receiveTestValue(t, closeDone, "Close result"); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if writerCloseCalls != 1 || filesCloseCalls != 1 {
		t.Fatalf("close calls = %d/%d, want 1/1", writerCloseCalls, filesCloseCalls)
	}
	result, err := logger.Record(t.Context(), LevelInfo, "later", nil)
	if result != (WriteResult{}) || !errors.Is(err, ErrClosed) {
		t.Fatalf("later Record() = (%#v, %v), want zero/ErrClosed", result, err)
	}
	gotEvents := events.snapshot()
	for _, pair := range [][2]string{
		{"file-write-enter", "write-release"},
		{"write-release", "file-write-exit"},
		{"file-write-exit", "writer-close"},
		{"writer-close", "files-close"},
	} {
		assertTestEventBefore(t, gotEvents, pair[0], pair[1])
	}
}

func TestLogger_LogPathWriteAndCloseAreRaceFree(t *testing.T) {
	logger, writer, files, _ := newCloseTestLogger(t, &closeSpy{})
	wantPath := logger.LogPath()
	events := &testEventLedger{}
	closeEntered := make(chan struct{})
	closeRelease := make(chan struct{})
	var closeEnteredOnce sync.Once
	var releaseOnce sync.Once
	releaseClose := func() {
		releaseOnce.Do(func() { close(closeRelease) })
	}
	t.Cleanup(releaseClose)
	writerCloseCalls := 0
	filesCloseCalls := 0
	writer.close = func() error {
		writerCloseCalls++
		events.add("writer-close-enter")
		closeEnteredOnce.Do(func() { close(closeEntered) })
		err := waitForTestRelease(closeRelease, "Logger Close writer release")
		events.add("writer-close-exit")
		return err
	}
	files.close = func() error {
		filesCloseCalls++
		events.add("files-close")
		return nil
	}

	closeDone := make(chan error, 1)
	go func() {
		err := logger.Close()
		events.add("close-return")
		closeDone <- err
	}()
	waitForTestSignal(t, closeEntered, "Logger Close writer entry")
	if logger.mu.TryLock() {
		logger.mu.Unlock()
		releaseClose()
		t.Fatal("Logger mutex is not held during writer Close")
	}

	const readers = 16
	paths := make(chan string, readers)
	var wait sync.WaitGroup
	for index := 0; index < readers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			paths <- logger.LogPath()
		}()
	}

	writeDone := make(chan struct {
		result WriteResult
		err    error
	}, 1)
	go func() {
		result, err := logger.Record(t.Context(), LevelInfo, "concurrent close", nil)
		writeDone <- struct {
			result WriteResult
			err    error
		}{result: result, err: err}
	}()
	if writerCloseCalls != 1 || filesCloseCalls != 0 {
		releaseClose()
		t.Fatalf(
			"close calls at writer barrier = %d/%d, want writer 1/files 0",
			writerCloseCalls,
			filesCloseCalls,
		)
	}

	events.add("writer-close-release")
	releaseClose()
	if err := receiveTestValue(t, closeDone, "Close result"); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	waitDone := make(chan struct{})
	go func() {
		wait.Wait()
		close(waitDone)
	}()
	waitForTestSignal(t, waitDone, "LogPath/write/Close concurrency")
	close(paths)
	for path := range paths {
		if path != wantPath {
			t.Fatalf("concurrent LogPath() = %q, want %q", path, wantPath)
		}
	}
	gotWrite := receiveTestValue(t, writeDone, "write after Close")
	if gotWrite.result != (WriteResult{}) || !errors.Is(gotWrite.err, ErrClosed) {
		t.Fatalf(
			"Record(concurrent Close) = (%#v, %v), want zero/ErrClosed",
			gotWrite.result,
			gotWrite.err,
		)
	}
	if writerCloseCalls != 1 || filesCloseCalls != 1 {
		t.Fatalf(
			"final close calls = writer %d/files %d, want 1/1",
			writerCloseCalls,
			filesCloseCalls,
		)
	}
	gotEvents := events.snapshot()
	for _, pair := range [][2]string{
		{"writer-close-enter", "writer-close-release"},
		{"writer-close-release", "writer-close-exit"},
		{"writer-close-exit", "files-close"},
		{"files-close", "close-return"},
	} {
		assertTestEventBefore(t, gotEvents, pair[0], pair[1])
	}
}
