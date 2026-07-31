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
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

type typedNilWriter struct{}

func (*typedNilWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

func validConstructorInputs(t *testing.T) (
	*config.Layout,
	time.Time,
	*fakeLogWriter,
	*fakeLogFiles,
	logFilesFactory,
) {
	t.Helper()
	layout := mustTestLayout(t)
	now := time.Date(2026, 7, 29, 18, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	path, err := layout.RuntimeLogFile("doctor", now)
	if err != nil {
		t.Fatalf("RuntimeLogFile() error = %v", err)
	}
	writer := fullFakeWriter(path)
	files := completeFakeFiles(writer)
	factory := func(context.Context, *config.Layout) (logFiles, error) {
		return files, nil
	}
	return layout, now, writer, files, factory
}

func cleanupConstructedLogger(t *testing.T, logger *Logger) {
	t.Helper()
	t.Cleanup(func() {
		logger.mu.Lock()
		defer logger.mu.Unlock()
		if err := closeOwned(logger.writer, logger.files); err != nil {
			t.Errorf("closeOwned() error = %v", err)
		}
	})
}

func assertErrorCausesInOrder(t *testing.T, err error, causes ...error) {
	t.Helper()
	var leaves []error
	var visit func(error)
	visit = func(current error) {
		switch unwrapped := current.(type) {
		case interface{ Unwrap() []error }:
			for _, child := range unwrapped.Unwrap() {
				visit(child)
			}
		case interface{ Unwrap() error }:
			visit(unwrapped.Unwrap())
		default:
			if current != nil {
				leaves = append(leaves, current)
			}
		}
	}
	visit(err)

	next := 0
	for _, cause := range causes {
		found := -1
		for index := next; index < len(leaves); index++ {
			if errors.Is(leaves[index], cause) {
				found = index
				break
			}
		}
		if found < 0 {
			t.Fatalf(
				"error leaves = %#v, want remaining ordered cause %v",
				leaves,
				cause,
			)
		}
		next = found + 1
	}
}

func TestNew_RejectsInvalidArguments(t *testing.T) {
	layout, now, _, _, validFactory := validConstructorInputs(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	var nilStderr *typedNilWriter
	validStderr := io.Writer(&bytes.Buffer{})
	validClock := func() time.Time { return now }
	validRetention := DefaultRetentionPolicy()

	tests := []struct {
		name        string
		ctx         context.Context
		layout      *config.Layout
		stderr      io.Writer
		command     string
		operationID string
		factory     logFilesFactory
		clock       func() time.Time
		retention   RetentionPolicy
		want        error
	}{
		{name: "nil context", layout: layout, stderr: validStderr, command: "doctor", operationID: "01JTEST", factory: validFactory, clock: validClock, retention: validRetention, want: ErrInvalidArgument},
		{name: "cancelled context", ctx: cancelled, layout: layout, stderr: validStderr, command: "doctor", operationID: "01JTEST", factory: validFactory, clock: validClock, retention: validRetention, want: context.Canceled},
		{name: "nil layout", ctx: t.Context(), stderr: validStderr, command: "doctor", operationID: "01JTEST", factory: validFactory, clock: validClock, retention: validRetention, want: ErrInvalidArgument},
		{name: "nil stderr", ctx: t.Context(), layout: layout, command: "doctor", operationID: "01JTEST", factory: validFactory, clock: validClock, retention: validRetention, want: ErrInvalidArgument},
		{name: "typed nil stderr", ctx: t.Context(), layout: layout, stderr: nilStderr, command: "doctor", operationID: "01JTEST", factory: validFactory, clock: validClock, retention: validRetention, want: ErrInvalidArgument},
		{name: "empty command", ctx: t.Context(), layout: layout, stderr: validStderr, operationID: "01JTEST", factory: validFactory, clock: validClock, retention: validRetention, want: ErrInvalidArgument},
		{name: "invalid command segment", ctx: t.Context(), layout: layout, stderr: validStderr, command: "nested/doctor", operationID: "01JTEST", factory: validFactory, clock: validClock, retention: validRetention, want: ErrInvalidArgument},
		{name: "command NUL", ctx: t.Context(), layout: layout, stderr: validStderr, command: "doctor\x00", operationID: "01JTEST", factory: validFactory, clock: validClock, retention: validRetention, want: ErrInvalidArgument},
		{name: "empty operation id", ctx: t.Context(), layout: layout, stderr: validStderr, command: "doctor", factory: validFactory, clock: validClock, retention: validRetention, want: ErrInvalidArgument},
		{name: "operation id NUL", ctx: t.Context(), layout: layout, stderr: validStderr, command: "doctor", operationID: "01J\x00", factory: validFactory, clock: validClock, retention: validRetention, want: ErrInvalidArgument},
		{name: "nil factory", ctx: t.Context(), layout: layout, stderr: validStderr, command: "doctor", operationID: "01JTEST", clock: validClock, retention: validRetention, want: ErrInvalidArgument},
		{name: "nil clock", ctx: t.Context(), layout: layout, stderr: validStderr, command: "doctor", operationID: "01JTEST", factory: validFactory, retention: validRetention, want: ErrInvalidArgument},
		{name: "zero clock", ctx: t.Context(), layout: layout, stderr: validStderr, command: "doctor", operationID: "01JTEST", factory: validFactory, clock: func() time.Time { return time.Time{} }, retention: validRetention, want: ErrInvalidTime},
		{name: "invalid retention", ctx: t.Context(), layout: layout, stderr: validStderr, command: "doctor", operationID: "01JTEST", factory: validFactory, clock: validClock, retention: RetentionPolicy{}, want: ErrInvalidRetention},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			factoryCalls := 0
			factory := test.factory
			if factory != nil {
				factory = func(ctx context.Context, layout *config.Layout) (logFiles, error) {
					factoryCalls++
					return test.factory(ctx, layout)
				}
			}
			logger, err := newWithDependencies(
				test.ctx,
				test.layout,
				test.stderr,
				test.command,
				test.operationID,
				factory,
				test.clock,
				test.retention,
			)
			if logger != nil {
				t.Fatalf("newWithDependencies() logger = %#v, want nil", logger)
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("newWithDependencies() error = %v, want errors.Is(_, %v)", err, test.want)
			}
			if factoryCalls != 0 {
				t.Fatalf("factory calls = %d, want 0", factoryCalls)
			}
		})
	}
}

func TestNew_ValidatesBeforeCreatingFilesystemCapability(t *testing.T) {
	layout, _, _, _, _ := validConstructorInputs(t)
	factoryCalls := 0
	_, err := newWithDependencies(
		t.Context(),
		layout,
		&bytes.Buffer{},
		"bad/name",
		"01JTEST",
		func(context.Context, *config.Layout) (logFiles, error) {
			factoryCalls++
			return nil, errors.New("factory must not run")
		},
		time.Now,
		DefaultRetentionPolicy(),
	)
	if !errors.Is(err, ErrInvalidArgument) || factoryCalls != 0 {
		t.Fatalf("result = (error %v, factory calls %d), want invalid argument before factory", err, factoryCalls)
	}
}

func TestNew_CreatesCurrentFileAndReturnsAbsoluteLogPath(t *testing.T) {
	layout, now, writer, files, _ := validConstructorInputs(t)
	var output bytes.Buffer
	clockCalls := 0
	openCalls := 0
	constructionCtx := t.Context()
	files.openAppend = func(ctx context.Context, command string, localDate time.Time) (logWriter, error) {
		openCalls++
		if ctx != constructionCtx {
			t.Fatalf("OpenAppend ctx identity changed during construction")
		}
		if command != "doctor" || !localDate.Equal(now) {
			t.Fatalf("OpenAppend(%q, %v), want doctor/%v", command, localDate, now)
		}
		return writer, nil
	}
	logger, err := newWithDependencies(
		constructionCtx,
		layout,
		&output,
		"doctor",
		"01JTEST",
		func(context.Context, *config.Layout) (logFiles, error) { return files, nil },
		func() time.Time {
			clockCalls++
			return now
		},
		DefaultRetentionPolicy(),
	)
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}
	cleanupConstructedLogger(t, logger)
	if !filepath.IsAbs(logger.LogPath()) || logger.LogPath() != writer.Path() {
		t.Fatalf("LogPath() = %q, want absolute %q", logger.LogPath(), writer.Path())
	}
	if clockCalls != 1 || openCalls != 1 {
		t.Fatalf("calls = clock %d/open %d, want 1/1", clockCalls, openCalls)
	}
}

func TestNew_RunsRetentionAcrossAllCommandsAndRecordsResult(t *testing.T) {
	layout := mustTestLayout(t)
	location := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, location)
	activePath, err := layout.RuntimeLogFile("doctor", now)
	if err != nil {
		t.Fatalf("RuntimeLogFile(active) error = %v", err)
	}
	var file bytes.Buffer
	writer := &fakeLogWriter{
		path: activePath,
		write: func(p []byte) (int, error) {
			return file.Write(p)
		},
		close: func() error { return nil },
	}
	var listed []retainedFile
	for _, command := range []string{"doctor", "workspace-sync"} {
		for day := 27; day <= 30; day++ {
			path, pathErr := layout.RuntimeLogFile(
				command,
				time.Date(2026, 7, day, 0, 0, 0, 0, location),
			)
			if pathErr != nil {
				t.Fatalf("RuntimeLogFile() error = %v", pathErr)
			}
			listed = append(listed, fakeRetainedFile{name: filepath.Base(path), path: path})
		}
	}
	var removed []string
	files := &fakeLogFiles{
		openAppend: func(context.Context, string, time.Time) (logWriter, error) {
			return writer, nil
		},
		list: func(context.Context) ([]retainedFile, error) {
			return listed, nil
		},
		remove: func(_ context.Context, retained retainedFile) (removeResult, error) {
			removed = append(removed, retained.Name())
			return removeResult{mutationApplied: true}, nil
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
		func() time.Time { return now },
		RetentionPolicy{MaxAgeDays: 30, MaxFilesPerCommand: 2},
	)
	if err != nil {
		t.Fatalf("newWithDependencies() error = %v", err)
	}
	cleanupConstructedLogger(t, logger)
	wantRemoved := []string{
		"doctor-20260727.log",
		"doctor-20260728.log",
		"workspace-sync-20260727.log",
		"workspace-sync-20260728.log",
	}
	if !reflect.DeepEqual(removed, wantRemoved) {
		t.Fatalf("removed = %#v, want %#v", removed, wantRemoved)
	}
	lines := bytes.Split(bytes.TrimSpace(file.Bytes()), []byte{'\n'})
	if len(lines) != 1 {
		t.Fatalf("maintenance line count = %d, want 1", len(lines))
	}
	var entry map[string]any
	if err := json.Unmarshal(lines[0], &entry); err != nil {
		t.Fatalf("decode maintenance entry: %v", err)
	}
	if entry["message"] != "日志保留清理完成" {
		t.Fatalf("maintenance message = %#v, want success message", entry["message"])
	}
}

func TestNew_PreservesFilesystemSecurityError(t *testing.T) {
	layout, now, _, _, _ := validConstructorInputs(t)
	securityErr := &filesystem.Error{
		Operation: "open",
		Path:      layout.RuntimeLogDir(),
		Err:       errors.New("unsafe reparse point"),
	}
	logger, err := newWithDependencies(
		t.Context(),
		layout,
		&bytes.Buffer{},
		"doctor",
		"01JTEST",
		func(context.Context, *config.Layout) (logFiles, error) {
			return nil, securityErr
		},
		func() time.Time { return now },
		DefaultRetentionPolicy(),
	)
	if logger != nil {
		t.Fatalf("newWithDependencies() logger = %#v, want nil", logger)
	}
	var got *filesystem.Error
	if !errors.As(err, &got) || got != securityErr {
		t.Fatalf("newWithDependencies() error = %v, want original *filesystem.Error", err)
	}
}

func TestNew_JoinsConstructionAndCloseErrors(t *testing.T) {
	layout, now, _, files, _ := validConstructorInputs(t)
	factoryErr := errors.New("factory")
	closeErr := errors.New("files close")
	closeCalls := 0
	files.close = func() error {
		closeCalls++
		return closeErr
	}
	logger, err := newWithDependencies(
		t.Context(),
		layout,
		&bytes.Buffer{},
		"doctor",
		"01JTEST",
		func(context.Context, *config.Layout) (logFiles, error) {
			return files, factoryErr
		},
		func() time.Time { return now },
		DefaultRetentionPolicy(),
	)
	if logger != nil {
		t.Fatalf("newWithDependencies() logger = %#v, want nil", logger)
	}
	if !errors.Is(err, factoryErr) || !errors.Is(err, closeErr) {
		t.Fatalf("newWithDependencies() error = %v, want factory and close causes", err)
	}
	if closeCalls != 1 {
		t.Fatalf("files Close calls = %d, want 1", closeCalls)
	}
}

func TestNew_CancelDuringRetentionListPreservesCausesAndCloses(t *testing.T) {
	layout := mustTestLayout(t)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.Local)
	activePath, err := layout.RuntimeLogFile("doctor", now)
	if err != nil {
		t.Fatalf("RuntimeLogFile(active) error = %v", err)
	}
	listErr := errors.New("list retained logs")
	writerCloseErr := errors.New("writer close after list cancellation")
	filesCloseErr := errors.New("files close after list cancellation")
	listEntered := make(chan struct{})
	listRelease := make(chan struct{})
	writerCloseCalls := 0
	filesCloseCalls := 0
	removeCalls := 0
	fileWriteCalls := 0
	stderrWriteCalls := 0
	writer := &fakeLogWriter{
		path: activePath,
		write: func(p []byte) (int, error) {
			fileWriteCalls++
			return len(p), nil
		},
		close: func() error {
			writerCloseCalls++
			return writerCloseErr
		},
	}
	files := &fakeLogFiles{
		openAppend: func(context.Context, string, time.Time) (logWriter, error) {
			return writer, nil
		},
		list: func(context.Context) ([]retainedFile, error) {
			close(listEntered)
			if releaseErr := waitForTestRelease(
				listRelease,
				"retention List release",
			); releaseErr != nil {
				return nil, errors.Join(listErr, releaseErr)
			}
			return nil, listErr
		},
		remove: func(context.Context, retainedFile) (removeResult, error) {
			removeCalls++
			return removeResult{}, nil
		},
		close: func() error {
			filesCloseCalls++
			return filesCloseErr
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	resultCh := make(chan struct {
		logger *Logger
		err    error
	}, 1)
	go func() {
		logger, constructorErr := newWithDependencies(
			ctx,
			layout,
			entryWriterFunc(func(p []byte) (int, error) {
				stderrWriteCalls++
				return len(p), nil
			}),
			"doctor",
			"01JTEST",
			func(context.Context, *config.Layout) (logFiles, error) {
				return files, nil
			},
			func() time.Time { return now },
			DefaultRetentionPolicy(),
		)
		resultCh <- struct {
			logger *Logger
			err    error
		}{logger: logger, err: constructorErr}
	}()
	waitForTestSignal(t, listEntered, "retention List entry")
	cancel()
	close(listRelease)
	got := receiveTestValue(t, resultCh, "List cancellation constructor result")
	if got.logger != nil {
		t.Fatalf("newWithDependencies() logger = %#v, want nil", got.logger)
	}
	for _, want := range []error{
		listErr,
		context.Canceled,
		writerCloseErr,
		filesCloseErr,
	} {
		if !errors.Is(got.err, want) {
			t.Fatalf("construction error = %v, want errors.Is(_, %v)", got.err, want)
		}
	}
	assertErrorCausesInOrder(
		t,
		got.err,
		listErr,
		context.Canceled,
		writerCloseErr,
		filesCloseErr,
	)
	if removeCalls != 0 || fileWriteCalls != 0 || stderrWriteCalls != 0 {
		t.Fatalf(
			"post-cancel side effects = remove %d/file %d/stderr %d, want zero",
			removeCalls,
			fileWriteCalls,
			stderrWriteCalls,
		)
	}
	if writerCloseCalls != 1 || filesCloseCalls != 1 {
		t.Fatalf(
			"close calls = writer %d/files %d, want 1/1",
			writerCloseCalls,
			filesCloseCalls,
		)
	}
}

func TestNew_DeadlineFromRetentionRemovePreservesCausesAndStops(t *testing.T) {
	layout := mustTestLayout(t)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.Local)
	activePath, err := layout.RuntimeLogFile("doctor", now)
	if err != nil {
		t.Fatalf("RuntimeLogFile(active) error = %v", err)
	}
	oldestPath, err := layout.RuntimeLogFile(
		"doctor",
		now.AddDate(0, 0, -41),
	)
	if err != nil {
		t.Fatalf("RuntimeLogFile(oldest) error = %v", err)
	}
	nextPath, err := layout.RuntimeLogFile(
		"doctor",
		now.AddDate(0, 0, -40),
	)
	if err != nil {
		t.Fatalf("RuntimeLogFile(next) error = %v", err)
	}
	removeErr := errors.New("remove retained log")
	writerCloseErr := errors.New("writer close after remove deadline")
	filesCloseErr := errors.New("files close after remove deadline")
	removeEntered := make(chan struct{})
	removeRelease := make(chan struct{})
	writerCloseCalls := 0
	filesCloseCalls := 0
	removeCalls := 0
	fileWriteCalls := 0
	stderrWriteCalls := 0
	var removed retainedFile
	writer := &fakeLogWriter{
		path: activePath,
		write: func(p []byte) (int, error) {
			fileWriteCalls++
			return len(p), nil
		},
		close: func() error {
			writerCloseCalls++
			return writerCloseErr
		},
	}
	files := &fakeLogFiles{
		openAppend: func(context.Context, string, time.Time) (logWriter, error) {
			return writer, nil
		},
		list: func(context.Context) ([]retainedFile, error) {
			return []retainedFile{
				fakeRetainedFile{name: filepath.Base(nextPath), path: nextPath},
				fakeRetainedFile{name: filepath.Base(oldestPath), path: oldestPath},
			}, nil
		},
		remove: func(
			_ context.Context,
			token retainedFile,
		) (removeResult, error) {
			removeCalls++
			if removeCalls == 1 {
				removed = token
				close(removeEntered)
				if releaseErr := waitForTestRelease(
					removeRelease,
					"retention Remove release",
				); releaseErr != nil {
					return removeResult{}, errors.Join(removeErr, releaseErr)
				}
			}
			return removeResult{}, errors.Join(
				removeErr,
				context.DeadlineExceeded,
			)
		},
		close: func() error {
			filesCloseCalls++
			return filesCloseErr
		},
	}
	resultCh := make(chan struct {
		logger *Logger
		err    error
	}, 1)
	go func() {
		logger, constructorErr := newWithDependencies(
			t.Context(),
			layout,
			entryWriterFunc(func(p []byte) (int, error) {
				stderrWriteCalls++
				return len(p), nil
			}),
			"doctor",
			"01JTEST",
			func(context.Context, *config.Layout) (logFiles, error) {
				return files, nil
			},
			func() time.Time { return now },
			DefaultRetentionPolicy(),
		)
		resultCh <- struct {
			logger *Logger
			err    error
		}{logger: logger, err: constructorErr}
	}()
	waitForTestSignal(t, removeEntered, "first retention Remove entry")
	close(removeRelease)
	got := receiveTestValue(t, resultCh, "Remove deadline constructor result")
	if got.logger != nil {
		t.Fatalf("newWithDependencies() logger = %#v, want nil", got.logger)
	}
	for _, want := range []error{
		removeErr,
		context.DeadlineExceeded,
		writerCloseErr,
		filesCloseErr,
	} {
		if !errors.Is(got.err, want) {
			t.Fatalf("construction error = %v, want errors.Is(_, %v)", got.err, want)
		}
	}
	assertErrorCausesInOrder(
		t,
		got.err,
		removeErr,
		context.DeadlineExceeded,
		writerCloseErr,
		filesCloseErr,
	)
	if removeCalls != 1 {
		t.Fatalf("Remove calls = %d, want 1", removeCalls)
	}
	if removed.Name() != filepath.Base(oldestPath) {
		t.Fatalf("removed token = %#v, want oldest selected token", removed)
	}
	if fileWriteCalls != 0 || stderrWriteCalls != 0 {
		t.Fatalf(
			"post-deadline writes = file %d/stderr %d, want zero",
			fileWriteCalls,
			stderrWriteCalls,
		)
	}
	if writerCloseCalls != 1 || filesCloseCalls != 1 {
		t.Fatalf(
			"close calls = writer %d/files %d, want 1/1",
			writerCloseCalls,
			filesCloseCalls,
		)
	}
}

func TestLogger_RetentionObjectReplacementMatrixPreservesErrorAndDoesNotRetry(t *testing.T) {
	tests := []struct {
		name            string
		mutationApplied bool
		cause           error
		countOnly       bool
	}{
		{
			name: "identity changed before mutation",
			cause: errors.Join(
				filesystem.ErrIdentityChanged,
				&filesystem.FileError{
					Operation: "remove",
					Path:      "replacement.log",
					Err:       errors.New("identity changed"),
				},
			),
		},
		{
			name:            "mutation applied before close error",
			mutationApplied: true,
			cause: &filesystem.FileError{
				Operation: "close",
				Path:      "removed.log",
				Err:       errors.New("close"),
			},
		},
		{
			name:      "identity changed after count-only selection",
			countOnly: true,
			cause: errors.Join(
				filesystem.ErrIdentityChanged,
				&filesystem.FileError{
					Operation: "remove",
					Path:      "count-replacement.log",
					Err:       errors.New("identity changed"),
				},
			),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout := mustTestLayout(t)
			now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.Local)
			activePath, err := layout.RuntimeLogFile("doctor", now)
			if err != nil {
				t.Fatalf("RuntimeLogFile(active) error = %v", err)
			}
			selectedDate := now.AddDate(0, 0, -40)
			retention := DefaultRetentionPolicy()
			if test.countOnly {
				selectedDate = now.AddDate(0, 0, -2)
				retention = RetentionPolicy{
					MaxAgeDays:         30,
					MaxFilesPerCommand: 2,
				}
			}
			selectedPath, err := layout.RuntimeLogFile("doctor", selectedDate)
			if err != nil {
				t.Fatalf("RuntimeLogFile(selected) error = %v", err)
			}
			original := fakeRetainedFile{
				name:     filepath.Base(selectedPath),
				path:     selectedPath,
				sentinel: "replacement must survive",
			}
			listed := []retainedFile{original}
			if test.countOnly {
				middlePath, pathErr := layout.RuntimeLogFile(
					"doctor",
					now.AddDate(0, 0, -1),
				)
				if pathErr != nil {
					t.Fatalf("RuntimeLogFile(middle) error = %v", pathErr)
				}
				listed = []retainedFile{
					fakeRetainedFile{
						name: filepath.Base(activePath),
						path: activePath,
					},
					fakeRetainedFile{
						name: filepath.Base(middlePath),
						path: middlePath,
					},
					original,
				}
			}
			var file bytes.Buffer
			var stderr bytes.Buffer
			removeCalls := 0
			var removedToken retainedFile
			writer := &fakeLogWriter{
				path:  activePath,
				write: func(p []byte) (int, error) { return file.Write(p) },
				close: func() error { return nil },
			}
			files := &fakeLogFiles{
				openAppend: func(context.Context, string, time.Time) (logWriter, error) {
					return writer, nil
				},
				list: func(context.Context) ([]retainedFile, error) {
					return listed, nil
				},
				remove: func(_ context.Context, token retainedFile) (removeResult, error) {
					removeCalls++
					removedToken = token
					return removeResult{mutationApplied: test.mutationApplied}, test.cause
				},
				close: func() error { return nil },
			}
			logger, err := newWithDependencies(
				t.Context(),
				layout,
				&stderr,
				"doctor",
				"01JTEST",
				func(context.Context, *config.Layout) (logFiles, error) { return files, nil },
				func() time.Time { return now },
				retention,
			)
			if err != nil {
				t.Fatalf("newWithDependencies() error = %v", err)
			}
			cleanupConstructedLogger(t, logger)
			if removeCalls != 1 || removedToken != original {
				t.Fatalf("Remove calls/token = %d/%#v, want one call with original token", removeCalls, removedToken)
			}
			if got, want := original.sentinel, "replacement must survive"; got != want {
				t.Fatalf("replacement sentinel = %q, want %q", got, want)
			}
			if !strings.Contains(stderr.String(), "日志保留清理存在失败") {
				t.Fatalf("stderr = %q, want one maintenance warning", stderr.String())
			}
			if got := strings.Count(stderr.String(), "\n"); got != 1 {
				t.Fatalf("stderr line count = %d, want 1", got)
			}
		})
	}
}

func TestNew_ClosesWriterAndFilesWhenMaintenanceWarningCannotBeWritten(t *testing.T) {
	layout := mustTestLayout(t)
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.Local)
	activePath, err := layout.RuntimeLogFile("doctor", now)
	if err != nil {
		t.Fatalf("RuntimeLogFile(active) error = %v", err)
	}
	oldPath, err := layout.RuntimeLogFile("doctor", now.AddDate(0, 0, -40))
	if err != nil {
		t.Fatalf("RuntimeLogFile(old) error = %v", err)
	}
	removeErr := errors.New("remove")
	fileWriteErr := errors.New("file write")
	stderrWriteErr := errors.New("stderr write")
	writerCloseErr := errors.New("writer close")
	filesCloseErr := errors.New("files close")
	writerCloseCalls := 0
	filesCloseCalls := 0
	writer := &fakeLogWriter{
		path: activePath,
		write: func([]byte) (int, error) {
			return 0, fileWriteErr
		},
		close: func() error {
			writerCloseCalls++
			return writerCloseErr
		},
	}
	files := &fakeLogFiles{
		openAppend: func(context.Context, string, time.Time) (logWriter, error) {
			return writer, nil
		},
		list: func(context.Context) ([]retainedFile, error) {
			return []retainedFile{
				fakeRetainedFile{name: filepath.Base(oldPath), path: oldPath},
			}, nil
		},
		remove: func(context.Context, retainedFile) (removeResult, error) {
			return removeResult{}, removeErr
		},
		close: func() error {
			filesCloseCalls++
			return filesCloseErr
		},
	}
	logger, err := newWithDependencies(
		t.Context(),
		layout,
		entryWriterFunc(func([]byte) (int, error) { return 0, stderrWriteErr }),
		"doctor",
		"01JTEST",
		func(context.Context, *config.Layout) (logFiles, error) { return files, nil },
		func() time.Time { return now },
		DefaultRetentionPolicy(),
	)
	if logger != nil {
		t.Fatalf("newWithDependencies() logger = %#v, want nil", logger)
	}
	for _, want := range []error{
		removeErr, fileWriteErr, stderrWriteErr, writerCloseErr, filesCloseErr,
	} {
		if !errors.Is(err, want) {
			t.Fatalf("construction error = %v, want errors.Is(_, %v)", err, want)
		}
	}
	if writerCloseCalls != 1 || filesCloseCalls != 1 {
		t.Fatalf("close calls = writer %d/files %d, want 1/1", writerCloseCalls, filesCloseCalls)
	}
}
