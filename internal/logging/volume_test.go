package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

func TestLogger_RecordFileSystem(t *testing.T) {
	for _, test := range []struct {
		name   string
		format string
		err    error
		want   string
	}{
		{name: "NTFS", format: "NTFS", want: "NTFS"},
		{name: "exFAT", format: "exFAT", want: "exFAT"},
		{name: "unavailable", format: "NTFS", err: errors.New("probe unavailable"), want: "unknown"},
		{name: "empty", want: "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			logger, _, _, file, stderr := newWritableLogger(t, fixedEntryTime)
			calls := 0
			if err := logger.recordFileSystem(t.Context(), func(ctx context.Context, path string) (string, error) {
				calls++
				if path != logger.layout.AppRoot() || ctx != t.Context() {
					t.Fatalf("probe(ctx, %q), want original context and %q", path, logger.layout.AppRoot())
				}
				return test.format, test.err
			}); err != nil {
				t.Fatalf("recordFileSystem() error = %v, want nil", err)
			}
			if calls != 1 {
				t.Fatalf("probe calls = %d, want 1", calls)
			}
			entry := decodeSingleLogLine(t, file.Bytes())
			var details map[string]string
			if err := json.Unmarshal(entry["details"], &details); err != nil {
				t.Fatal(err)
			}
			if details["fileSystem"] != test.want || details["path"] != logger.layout.AppRoot() {
				t.Fatalf("details = %#v, want fileSystem %q/path %q", details, test.want, logger.layout.AppRoot())
			}
			if string(entry["level"]) != `"info"` || string(entry["kind"]) != `"diagnostic"` {
				t.Fatalf("entry = %s, want info diagnostic", file.Bytes())
			}
			if !strings.Contains(stderr.String(), "当前磁盘格式："+test.want) || bytes.Count(stderr.Bytes(), []byte{'\n'}) != 1 {
				t.Fatalf("stderr = %q, want one format diagnostic", stderr.String())
			}
		})
	}
	t.Run("write failure", func(t *testing.T) {
		logger, _, writer, _, stderr := newWritableLogger(t, fixedEntryTime)
		cause := errors.New("log write unavailable")
		writer.write = func([]byte) (int, error) { return 0, cause }
		err := logger.recordFileSystem(t.Context(), func(context.Context, string) (string, error) { return "NTFS", nil })
		if !errors.Is(err, cause) || !strings.Contains(stderr.String(), "NTFS") {
			t.Fatalf("recordFileSystem() = %v, stderr = %q, want sink failure and stderr diagnostic", err, stderr.String())
		}
	})
}

func TestNew_RecordsFileSystem(t *testing.T) {
	layout := mustTestLayout(t)
	if err := os.Mkdir(layout.AppRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	format, err := filesystem.FileSystemName(t.Context(), layout.AppRoot())
	if err != nil || format == "" {
		t.Fatalf("FileSystemName() = %q, %v, want actual volume format", format, err)
	}
	var stderr bytes.Buffer
	logger, err := New(t.Context(), layout, &stderr, "doctor", "disk-format-test")
	if err != nil {
		t.Fatal(err)
	}
	path := logger.LogPath()
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range bytes.Split(bytes.TrimSpace(content), []byte{'\n'}) {
		var entry struct {
			Message string
			Details map[string]any
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Message != "当前磁盘格式："+format {
			continue
		}
		count++
		if entry.Details["fileSystem"] != format || entry.Details["path"] != layout.AppRoot() {
			t.Fatalf("details = %#v, want actual format %q and app root", entry.Details, format)
		}
	}
	if count != 1 || !strings.Contains(stderr.String(), "当前磁盘格式："+format) {
		t.Fatalf("format entries = %d, stderr = %q, want exactly one diagnostic", count, stderr.String())
	}
}
