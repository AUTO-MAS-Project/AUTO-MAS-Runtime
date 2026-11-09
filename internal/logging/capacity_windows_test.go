package logging

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

func capacityLayout(t *testing.T) (*config.Layout, time.Time) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "app")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	layout, err := config.NewLayout(root, base)
	if err != nil {
		t.Fatal(err)
	}
	return layout, time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
}

func capacityLogger(t *testing.T, layout *config.Layout, now time.Time, options ...Option) *Logger {
	t.Helper()
	options = append(options, WithClock(func() time.Time { return now }))
	logger, err := New(t.Context(), layout, io.Discard, "backend supervise", "capacity-test", options...)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := logger.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return logger
}

func fillCapacityLog(t *testing.T, path string) {
	t.Helper()
	if err := os.Truncate(path, filesystem.MaxRuntimeLogBytes); err != nil {
		t.Fatal(err)
	}
}

func requireLogMessages(t *testing.T, path string, messages ...string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]int)
	for _, line := range bytes.Split(bytes.TrimSpace(content), []byte{'\n'}) {
		var record struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("Unmarshal() error = %v, want complete JSON line", err)
		}
		found[record.Message]++
	}
	for _, message := range messages {
		if found[message] != 1 {
			t.Fatalf("message %q count = %d, want 1", message, found[message])
		}
	}
}

func TestLoggerWindows_CapacityRotation(t *testing.T) {
	for _, startup := range []bool{false, true} {
		t.Run(fmt.Sprintf("startup=%t", startup), func(t *testing.T) {
			layout, now := capacityLayout(t)
			logger := capacityLogger(t, layout, now)
			oldPath := logger.LogPath()
			fillCapacityLog(t, oldPath)
			if startup {
				if err := logger.Close(); err != nil {
					t.Fatal(err)
				}
				logger = capacityLogger(t, layout, now)
			}
			message := strings.Repeat("完整记录", 1000)
			result, err := logger.Record(t.Context(), LevelInfo, message, nil)
			if err != nil || !result.FileWritten || (!startup && !result.Rotated) {
				t.Fatalf("Record() = (%+v, %v), want complete capacity rotation", result, err)
			}
			wantPath := strings.TrimSuffix(oldPath, ".log") + ".001.log"
			if logger.LogPath() != wantPath {
				t.Fatalf("LogPath() = %q, want %q", logger.LogPath(), wantPath)
			}
			requireLogMessages(t, wantPath, message)
			info, err := os.Stat(oldPath)
			if err != nil || info.Size() != filesystem.MaxRuntimeLogBytes {
				t.Fatalf("old file = %v, %v, want unchanged capacity", info, err)
			}
		})
	}
}

func TestLoggerWindows_ReopensLatestSegment(t *testing.T) {
	layout, now := capacityLayout(t)
	logger := capacityLogger(t, layout, now)
	basePath := logger.LogPath()
	fillCapacityLog(t, basePath)
	if _, err := logger.Record(t.Context(), LevelInfo, "first segment", nil); err != nil {
		t.Fatal(err)
	}
	latest := logger.LogPath()
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(basePath); err != nil {
		t.Fatal(err)
	}
	logger = capacityLogger(t, layout, now)
	if logger.LogPath() != latest {
		t.Fatalf("LogPath() = %q, want latest %q", logger.LogPath(), latest)
	}
	if _, err := os.Stat(basePath); !os.IsNotExist(err) {
		t.Fatalf("Stat(base) error = %v, want not exist", err)
	}
	fillCapacityLog(t, latest)
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	logger = capacityLogger(t, layout, now)
	want := strings.TrimSuffix(basePath, ".log") + ".002.log"
	if logger.LogPath() != want {
		t.Fatalf("LogPath() = %q, want %q", logger.LogPath(), want)
	}
}

func TestLoggerWindows_SegmentRetention(t *testing.T) {
	layout, now := capacityLayout(t)
	policy := WithRetention(RetentionPolicy{MaxAgeDays: 30, MaxFilesPerCommand: 2})
	logger := capacityLogger(t, layout, now, policy)
	oldest := logger.LogPath()
	for range 2 {
		fillCapacityLog(t, logger.LogPath())
		if err := logger.Close(); err != nil {
			t.Fatal(err)
		}
		logger = capacityLogger(t, layout, now, policy)
	}
	entries, err := os.ReadDir(layout.RuntimeLogDir())
	if err != nil || len(entries) != 2 {
		t.Fatalf("ReadDir() = %d files, %v, want 2", len(entries), err)
	}
	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Fatalf("Stat(oldest) error = %v, want not exist", err)
	}
}

func TestLoggerWindows_ConcurrentCapacityRotation(t *testing.T) {
	layout, now := capacityLayout(t)
	logger := capacityLogger(t, layout, now)
	fillCapacityLog(t, logger.LogPath())
	const count = 16
	start := make(chan struct{})
	failures := make(chan error, count)
	var group sync.WaitGroup
	for index := range count {
		group.Go(func() {
			<-start
			_, err := logger.Record(t.Context(), LevelInfo, fmt.Sprintf("record-%d", index), nil)
			failures <- err
		})
	}
	close(start)
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatalf("Record() error = %v, want nil", err)
		}
	}
	messages := make([]string, 0, count)
	for index := range count {
		messages = append(messages, fmt.Sprintf("record-%d", index))
	}
	requireLogMessages(t, logger.LogPath(), messages...)
}

func TestFilesystemLogFiles_ConcurrentNextSegments(t *testing.T) {
	files, _ := newRealLogFiles(t)
	const count = 8
	start := make(chan struct{})
	results := make(chan struct {
		writer logWriter
		err    error
	}, count)
	var group sync.WaitGroup
	for range count {
		group.Go(func() {
			<-start
			writer, err := files.OpenNext(t.Context(), "doctor", fixedEntryTime())
			results <- struct {
				writer logWriter
				err    error
			}{writer: writer, err: err}
		})
	}
	close(start)
	group.Wait()
	close(results)
	paths := make(map[string]bool)
	for result := range results {
		if result.err != nil {
			t.Errorf("OpenNext() error = %v, want nil", result.err)
			continue
		}
		if paths[result.writer.Path()] {
			t.Errorf("OpenNext() reused %q", result.writer.Path())
		}
		paths[result.writer.Path()] = true
		if err := result.writer.Close(); err != nil {
			t.Error(err)
		}
	}
	if len(paths) != count {
		t.Fatalf("unique segments = %d, want %d", len(paths), count)
	}
}

func TestLoggerWindows_DateRotationReopensLatestSegment(t *testing.T) {
	layout, now := capacityLayout(t)
	logger := capacityLogger(t, layout, now)
	tomorrow := now.AddDate(0, 0, 1)
	files := logger.files.(*filesystemLogFiles)
	writer, err := files.files.CreateSegment(t.Context(), "backend supervise", tomorrow, 7)
	if err != nil {
		t.Fatal(err)
	}
	path := writer.Path()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	logger.clock = func() time.Time { return tomorrow }
	result, err := logger.Record(t.Context(), LevelInfo, "next day", nil)
	if err != nil || !result.Rotated || logger.LogPath() != path {
		t.Fatalf("Record() = %+v, %v, path %q, want latest next-day segment %q", result, err, logger.LogPath(), path)
	}
	requireLogMessages(t, path, "next day")
}
