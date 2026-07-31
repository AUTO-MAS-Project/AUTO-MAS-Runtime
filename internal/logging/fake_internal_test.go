package logging

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
)

type fakeLogFiles struct {
	openAppend func(context.Context, string, time.Time) (logWriter, error)
	list       func(context.Context) ([]retainedFile, error)
	remove     func(context.Context, retainedFile) (removeResult, error)
	close      func() error
}

func (f *fakeLogFiles) OpenAppend(
	ctx context.Context,
	command string,
	localDate time.Time,
) (logWriter, error) {
	if f.openAppend == nil {
		return nil, errors.New("fake open append is not configured")
	}
	return f.openAppend(ctx, command, localDate)
}

func (f *fakeLogFiles) List(ctx context.Context) ([]retainedFile, error) {
	if f.list == nil {
		return nil, errors.New("fake list is not configured")
	}
	return f.list(ctx)
}

func (f *fakeLogFiles) Remove(
	ctx context.Context,
	file retainedFile,
) (removeResult, error) {
	if f.remove == nil {
		return removeResult{}, errors.New("fake remove is not configured")
	}
	return f.remove(ctx, file)
}

func (f *fakeLogFiles) Close() error {
	if f.close == nil {
		return errors.New("fake files close is not configured")
	}
	return f.close()
}

type fakeLogWriter struct {
	write func([]byte) (int, error)
	path  string
	close func() error
}

func (w *fakeLogWriter) Write(p []byte) (int, error) {
	if w.write == nil {
		return 0, errors.New("fake writer write is not configured")
	}
	return w.write(p)
}

func (w *fakeLogWriter) Path() string {
	return w.path
}

func (w *fakeLogWriter) Close() error {
	if w.close == nil {
		return errors.New("fake writer close is not configured")
	}
	return w.close()
}

type fakeRetainedFile struct {
	name     string
	path     string
	sentinel string
}

func (f fakeRetainedFile) Name() string {
	return f.name
}

func (f fakeRetainedFile) Path() string {
	return f.path
}

func (fakeRetainedFile) retainedLogFile() {}

const testBarrierTimeout = 5 * time.Second

func waitForTestSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	timer := time.NewTimer(testBarrierTimeout)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatalf("timeout waiting for %s", name)
	}
}

func receiveTestValue[T any](t *testing.T, values <-chan T, name string) T {
	t.Helper()
	timer := time.NewTimer(testBarrierTimeout)
	defer timer.Stop()
	select {
	case value := <-values:
		return value
	case <-timer.C:
		t.Fatalf("timeout waiting for %s", name)
		var zero T
		return zero
	}
}

func waitForTestRelease(signal <-chan struct{}, name string) error {
	timer := time.NewTimer(testBarrierTimeout)
	defer timer.Stop()
	select {
	case <-signal:
		return nil
	case <-timer.C:
		return fmt.Errorf("timeout waiting for %s", name)
	}
}

func mustTestLayout(t *testing.T) *config.Layout {
	t.Helper()
	base := t.TempDir()
	layout, err := config.NewLayout(filepath.Join(base, "app"), base)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	return layout
}

func fullFakeWriter(path string) *fakeLogWriter {
	return &fakeLogWriter{
		path: path,
		write: func(p []byte) (int, error) {
			return len(p), nil
		},
		close: func() error {
			return nil
		},
	}
}

func completeFakeFiles(writer logWriter) *fakeLogFiles {
	return &fakeLogFiles{
		openAppend: func(context.Context, string, time.Time) (logWriter, error) {
			return writer, nil
		},
		list: func(context.Context) ([]retainedFile, error) {
			return nil, nil
		},
		remove: func(context.Context, retainedFile) (removeResult, error) {
			return removeResult{}, nil
		},
		close: func() error {
			return nil
		},
	}
}

func TestFakeLogFiles_ImplementsTheCompleteConsumerSeam(t *testing.T) {
	var _ logFiles = (*fakeLogFiles)(nil)
	var _ logWriter = (*fakeLogWriter)(nil)
	var _ retainedFile = fakeRetainedFile{}
	var _ logFiles = (*filesystemLogFiles)(nil)
	var _ logWriter = (*filesystemLogWriter)(nil)
	var _ retainedFile = filesystemRetainedFile{}

	files := completeFakeFiles(fullFakeWriter(filepath.Join(t.TempDir(), "runtime.log")))
	writer, err := files.OpenAppend(t.Context(), "doctor", fixedEntryTime())
	if err != nil {
		t.Fatalf("OpenAppend() error = %v", err)
	}
	if _, err := writer.Write([]byte("line\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if _, err := files.List(t.Context()); err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if _, err := files.Remove(t.Context(), fakeRetainedFile{name: "old.log", path: "old.log"}); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer Close() error = %v", err)
	}
	if err := files.Close(); err != nil {
		t.Fatalf("files Close() error = %v", err)
	}
}
