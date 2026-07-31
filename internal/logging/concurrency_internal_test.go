package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

type testEventLedger struct {
	mu     sync.Mutex
	events []string
}

func (l *testEventLedger) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *testEventLedger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func assertTestEventBefore(
	t *testing.T,
	events []string,
	before string,
	after string,
) {
	t.Helper()
	beforeIndex := -1
	afterIndex := -1
	for index, event := range events {
		switch event {
		case before:
			if beforeIndex >= 0 {
				t.Fatalf("event %q appears more than once in %#v", before, events)
			}
			beforeIndex = index
		case after:
			if afterIndex >= 0 {
				t.Fatalf("event %q appears more than once in %#v", after, events)
			}
			afterIndex = index
		}
	}
	if beforeIndex < 0 || afterIndex < 0 || beforeIndex >= afterIndex {
		t.Fatalf(
			"event order = %#v, want %q before %q",
			events,
			before,
			after,
		)
	}
}

func TestLogger_ConcurrentWritesRemainWholeAndOrdered(t *testing.T) {
	now := fixedEntryTime()
	logger, _, _, fileOutput, stderrOutput := newWritableLogger(t, func() time.Time { return now })
	const goroutines = 32
	levels := []Level{LevelDebug, LevelInfo, LevelWarn, LevelError}
	type expectedRecord struct {
		level      Level
		kind       entryKind
		message    string
		diagnostic bool
	}
	expected := make(map[int]expectedRecord, goroutines)
	expectedStderr := make(map[string]bool, goroutines/2)
	for index := 0; index < goroutines; index++ {
		level := levels[(index/2)%len(levels)]
		diagnostic := index%2 == 1
		kind := entryOperation
		if diagnostic {
			kind = entryDiagnostic
		}
		message := fmt.Sprintf("concurrent-%d", index)
		expected[index] = expectedRecord{
			level:      level,
			kind:       kind,
			message:    message,
			diagnostic: diagnostic,
		}
		if diagnostic {
			line := fmt.Sprintf(
				"%s %s [doctor] [01JTEST] %s {\"index\":%d}\n",
				now.Format(time.RFC3339Nano),
				strings.ToUpper(level.String()),
				message,
				index,
			)
			expectedStderr[line] = true
		}
	}

	start := make(chan struct{})
	errorsCh := make(chan error, goroutines)
	var wait sync.WaitGroup
	for index := 0; index < goroutines; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			test := expected[index]
			var err error
			if test.diagnostic {
				_, err = logger.Diagnostic(
					t.Context(),
					test.level,
					test.message,
					map[string]any{"index": index},
				)
			} else {
				_, err = logger.Record(
					t.Context(),
					test.level,
					test.message,
					map[string]any{"index": index},
				)
			}
			errorsCh <- err
		}()
	}
	close(start)
	waitDone := make(chan struct{})
	go func() {
		wait.Wait()
		close(waitDone)
	}()
	waitForTestSignal(t, waitDone, "concurrent writes")
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("Record() error = %v", err)
		}
	}

	lines := bytes.Split(bytes.TrimSuffix(fileOutput.Bytes(), []byte{'\n'}), []byte{'\n'})
	if len(lines) != goroutines {
		t.Fatalf("line count = %d, want %d", len(lines), goroutines)
	}
	seen := make(map[int]bool, goroutines)
	seenKindLevel := make(map[string]bool, len(levels)*2)
	for position, line := range lines {
		var decoded struct {
			Level   Level     `json:"level"`
			Kind    entryKind `json:"kind"`
			Command string    `json:"command"`
			Message string    `json:"message"`
			Details struct {
				Index int `json:"index"`
			} `json:"details"`
		}
		if err := json.Unmarshal(line, &decoded); err != nil {
			t.Fatalf("line %d is interleaved or invalid: %v: %q", position, err, line)
		}
		if seen[decoded.Details.Index] {
			t.Fatalf("index %d appears more than once", decoded.Details.Index)
		}
		seen[decoded.Details.Index] = true
		want, ok := expected[decoded.Details.Index]
		if !ok {
			t.Fatalf("line %d index = %d, want a submitted index", position, decoded.Details.Index)
		}
		if decoded.Level != want.level ||
			decoded.Kind != want.kind ||
			decoded.Command != "doctor" ||
			decoded.Message != want.message {
			t.Fatalf(
				"line %d = level %q kind %q command %q message %q, want %q/%q/doctor/%q",
				position,
				decoded.Level,
				decoded.Kind,
				decoded.Command,
				decoded.Message,
				want.level,
				want.kind,
				want.message,
			)
		}
		seenKindLevel[string(decoded.Kind)+"/"+decoded.Level.String()] = true
	}
	for _, kind := range []entryKind{entryOperation, entryDiagnostic} {
		for _, level := range levels {
			key := string(kind) + "/" + level.String()
			if !seenKindLevel[key] {
				t.Fatalf("file is missing concurrent kind/level %q", key)
			}
		}
	}

	stderrLines := bytes.Split(
		bytes.TrimSuffix(stderrOutput.Bytes(), []byte{'\n'}),
		[]byte{'\n'},
	)
	if len(stderrLines) != goroutines/2 {
		t.Fatalf("stderr line count = %d, want %d", len(stderrLines), goroutines/2)
	}
	seenStderr := make(map[string]bool, len(stderrLines))
	for position, line := range stderrLines {
		wholeLine := string(line) + "\n"
		if !expectedStderr[wholeLine] {
			t.Fatalf("stderr line %d is interleaved or unexpected: %q", position, wholeLine)
		}
		if seenStderr[wholeLine] {
			t.Fatalf("stderr line %d appears more than once: %q", position, wholeLine)
		}
		seenStderr[wholeLine] = true
	}
}

func TestLogger_RotationCallbacksHoldMutex(t *testing.T) {
	layout := mustTestLayout(t)
	dayOne := time.Date(2026, 7, 29, 23, 59, 59, 0, time.Local)
	dayTwo := dayOne.AddDate(0, 0, 1)
	oldPath, _ := layout.RuntimeLogFile("doctor", dayOne)
	newPath, _ := layout.RuntimeLogFile("doctor", dayTwo)
	oldWriter := fullFakeWriter(oldPath)
	newWriter := fullFakeWriter(newPath)
	events := &testEventLedger{}
	openEntered := make(chan struct{})
	openRelease := make(chan struct{})
	closeEntered := make(chan struct{})
	closeRelease := make(chan struct{})
	var openEnteredOnce sync.Once
	var closeEnteredOnce sync.Once
	var openReleaseOnce sync.Once
	var closeReleaseOnce sync.Once
	releaseOpen := func() {
		openReleaseOnce.Do(func() { close(openRelease) })
	}
	releaseClose := func() {
		closeReleaseOnce.Do(func() { close(closeRelease) })
	}
	oldWriter.close = func() error {
		events.add("old-close-enter")
		closeEnteredOnce.Do(func() { close(closeEntered) })
		err := waitForTestRelease(closeRelease, "old writer Close release")
		events.add("old-close-exit")
		return err
	}
	openCalls := 0
	files := &fakeLogFiles{
		openAppend: func(context.Context, string, time.Time) (logWriter, error) {
			openCalls++
			if openCalls == 1 {
				return oldWriter, nil
			}
			events.add("rotation-open-enter")
			openEnteredOnce.Do(func() { close(openEntered) })
			if err := waitForTestRelease(openRelease, "rotation OpenAppend release"); err != nil {
				return nil, err
			}
			events.add("rotation-open-exit")
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
		&bytes.Buffer{},
		"doctor",
		"01JTEST",
		func(context.Context, *config.Layout) (logFiles, error) { return files, nil },
		func() time.Time { return dayOne },
		DefaultRetentionPolicy(),
	)
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}
	logger.clock = func() time.Time { return dayTwo }
	cleanupConstructedLogger(t, logger)
	t.Cleanup(releaseClose)
	t.Cleanup(releaseOpen)

	resultCh := make(chan struct {
		result WriteResult
		err    error
	}, 1)
	go func() {
		result, writeErr := logger.Record(t.Context(), LevelInfo, "rotate", nil)
		resultCh <- struct {
			result WriteResult
			err    error
		}{result: result, err: writeErr}
	}()
	waitForTestSignal(t, openEntered, "rotation OpenAppend entry")
	if logger.mu.TryLock() {
		logger.mu.Unlock()
		releaseOpen()
		releaseClose()
		t.Fatal("Logger mutex is not held during rotation OpenAppend")
	}

	events.add("open-release")
	releaseOpen()
	waitForTestSignal(t, closeEntered, "old writer Close entry")
	if logger.mu.TryLock() {
		logger.mu.Unlock()
		releaseClose()
		t.Fatal("Logger mutex is not held during old writer Close")
	}
	events.add("close-release")
	releaseClose()

	got := receiveTestValue(t, resultCh, "rotation callback result")
	if got.err != nil || !got.result.Rotated || !got.result.FileWritten {
		t.Fatalf(
			"Record() = (%#v, %v), want rotated and file-written",
			got.result,
			got.err,
		)
	}
	if openCalls != 2 {
		t.Fatalf(
			"OpenAppend calls = %d, want construction plus one rotation",
			openCalls,
		)
	}
	if got := logger.LogPath(); got != newPath {
		t.Fatalf("final LogPath() = %q, want %q", got, newPath)
	}
	gotEvents := events.snapshot()
	for _, pair := range [][2]string{
		{"rotation-open-enter", "open-release"},
		{"open-release", "rotation-open-exit"},
		{"rotation-open-exit", "old-close-enter"},
		{"old-close-enter", "close-release"},
		{"close-release", "old-close-exit"},
	} {
		assertTestEventBefore(t, gotEvents, pair[0], pair[1])
	}
}

func TestLogger_ConcurrentRotationOpensOneWriter(t *testing.T) {
	layout := mustTestLayout(t)
	dayOne := time.Date(2026, 7, 29, 23, 59, 59, 0, time.Local)
	dayTwo := dayOne.AddDate(0, 0, 1)
	oldPath, _ := layout.RuntimeLogFile("doctor", dayOne)
	newPath, _ := layout.RuntimeLogFile("doctor", dayTwo)
	oldWriter := fullFakeWriter(oldPath)
	newWriter := fullFakeWriter(newPath)
	var openCallsMu sync.Mutex
	openCalls := 0
	files := &fakeLogFiles{
		openAppend: func(context.Context, string, time.Time) (logWriter, error) {
			openCallsMu.Lock()
			defer openCallsMu.Unlock()
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
		&bytes.Buffer{},
		"doctor",
		"01JTEST",
		func(context.Context, *config.Layout) (logFiles, error) { return files, nil },
		func() time.Time { return dayOne },
		DefaultRetentionPolicy(),
	)
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}
	logger.clock = func() time.Time { return dayTwo }
	cleanupConstructedLogger(t, logger)

	const goroutines = 32
	start := make(chan struct{})
	results := make(chan WriteResult, goroutines)
	errorsCh := make(chan error, goroutines)
	var wait sync.WaitGroup
	for index := 0; index < goroutines; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, writeErr := logger.Record(
				t.Context(),
				LevelInfo,
				"concurrent rotate",
				nil,
			)
			results <- result
			errorsCh <- writeErr
		}()
	}
	close(start)
	waitDone := make(chan struct{})
	go func() {
		wait.Wait()
		close(waitDone)
	}()
	waitForTestSignal(t, waitDone, "concurrent rotation writes")
	close(results)
	close(errorsCh)

	rotated := 0
	for result := range results {
		if !result.FileWritten {
			t.Fatalf("Record() result = %#v, want FileWritten", result)
		}
		if result.Rotated {
			rotated++
		}
	}
	for writeErr := range errorsCh {
		if writeErr != nil {
			t.Fatalf("Record() error = %v", writeErr)
		}
	}
	openCallsMu.Lock()
	gotOpenCalls := openCalls
	openCallsMu.Unlock()
	if rotated != 1 || gotOpenCalls != 2 {
		t.Fatalf(
			"rotated/OpenAppend calls = %d/%d, want 1/2",
			rotated,
			gotOpenCalls,
		)
	}
	if got := logger.LogPath(); got != newPath {
		t.Fatalf("LogPath() = %q, want %q", got, newPath)
	}
}

func TestLogger_SinkCallbackHoldsMutexAndConcurrentResultsRemainWhole(t *testing.T) {
	now := fixedEntryTime()
	logger, _, writer, fileOutput, _ := newWritableLogger(t, func() time.Time { return now })
	events := &testEventLedger{}
	firstEntered := make(chan struct{})
	firstRelease := make(chan struct{})
	var firstReleaseOnce sync.Once
	releaseFirst := func() {
		firstReleaseOnce.Do(func() { close(firstRelease) })
	}
	t.Cleanup(releaseFirst)
	var writeCountMu sync.Mutex
	writeCalls := 0
	writer.write = func(p []byte) (int, error) {
		writeCountMu.Lock()
		writeCalls++
		call := writeCalls
		writeCountMu.Unlock()
		events.add(fmt.Sprintf("file-write-%d-enter", call))
		if call == 1 {
			close(firstEntered)
			if err := waitForTestRelease(firstRelease, "first sink release"); err != nil {
				return 0, err
			}
		}
		n, err := fileOutput.Write(p)
		events.add(fmt.Sprintf("file-write-%d-exit", call))
		return n, err
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := logger.Record(t.Context(), LevelInfo, "first", nil)
		firstDone <- err
	}()
	waitForTestSignal(t, firstEntered, "first sink entry")
	if logger.mu.TryLock() {
		logger.mu.Unlock()
		releaseFirst()
		t.Fatal("Logger mutex is not held during file sink callback")
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := logger.Record(t.Context(), LevelInfo, "second", nil)
		secondDone <- err
	}()
	pathDone := make(chan string, 1)
	go func() {
		pathDone <- logger.LogPath()
	}()

	events.add("first-release")
	releaseFirst()
	if err := receiveTestValue(t, firstDone, "first record result"); err != nil {
		t.Fatalf("first Record() error = %v", err)
	}
	if err := receiveTestValue(t, secondDone, "second record result"); err != nil {
		t.Fatalf("second Record() error = %v", err)
	}
	path := receiveTestValue(t, pathDone, "LogPath result")
	finalPath := logger.LogPath()
	if path != finalPath {
		t.Fatalf("concurrent LogPath() = %q, want %q", path, finalPath)
	}
	writeCountMu.Lock()
	gotWriteCalls := writeCalls
	writeCountMu.Unlock()
	if gotWriteCalls != 2 {
		t.Fatalf("write calls = %d, want 2", gotWriteCalls)
	}
	lines := bytes.Split(bytes.TrimSuffix(fileOutput.Bytes(), []byte{'\n'}), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("line count = %d, want 2", len(lines))
	}
	var messages []string
	for _, line := range lines {
		var decoded struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(line, &decoded); err != nil {
			t.Fatalf("decode line: %v", err)
		}
		messages = append(messages, decoded.Message)
	}
	if want := []string{"first", "second"}; !reflect.DeepEqual(messages, want) {
		t.Fatalf("messages = %#v, want %#v", messages, want)
	}
	gotEvents := events.snapshot()
	for _, pair := range [][2]string{
		{"file-write-1-enter", "first-release"},
		{"first-release", "file-write-1-exit"},
		{"file-write-1-exit", "file-write-2-enter"},
		{"file-write-2-enter", "file-write-2-exit"},
	} {
		assertTestEventBefore(t, gotEvents, pair[0], pair[1])
	}
}
