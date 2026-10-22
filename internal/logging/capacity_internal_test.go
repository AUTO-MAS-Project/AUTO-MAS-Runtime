package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

func TestLogger_CapacityRotationFailure(t *testing.T) {
	sentinel := errors.New("injected I/O failure")
	for _, name := range []string{"write", "partial", "create", "invalid", "older", "close", "replay"} {
		t.Run(name, func(t *testing.T) {
			now := fixedEntryTime()
			logger, files, previous, _, _ := newWritableLogger(t, func() time.Time { return now })
			previous.write = func([]byte) (int, error) { return 0, filesystem.ErrRuntimeLogCapacity }
			nextPath, err := logger.layout.RuntimeLogSegmentFile("doctor", now, 1)
			if err != nil {
				t.Fatal(err)
			}
			next := fullFakeWriter(nextPath)
			openCalls := 0
			files.openNext = func(context.Context, string, time.Time) (logWriter, error) {
				openCalls++
				if name == "create" {
					return nil, sentinel
				}
				return next, nil
			}
			wantErr := sentinel
			wantOpen, wantRotated, wantWritten := 1, false, false
			switch name {
			case "write":
				previous.write = func([]byte) (int, error) { return 0, sentinel }
				wantOpen = 0
			case "partial":
				previous.write = func([]byte) (int, error) { return 1, filesystem.ErrRuntimeLogCapacity }
				wantErr, wantOpen, wantWritten = filesystem.ErrRuntimeLogCapacity, 0, true
			case "invalid":
				next.path = previous.Path()
				wantErr = ErrInvalidArgument
			case "older":
				logger.activePath, err = logger.layout.RuntimeLogSegmentFile("doctor", now, 2)
				if err != nil {
					t.Fatal(err)
				}
				previous.path = logger.activePath
				wantErr = ErrInvalidArgument
			case "close":
				previous.close = func() error { return sentinel }
				wantRotated, wantWritten = true, true
			case "replay":
				next.write = func([]byte) (int, error) { return 0, sentinel }
				wantRotated = true
			}
			result, err := logger.Record(t.Context(), LevelInfo, "record", nil)
			if !errors.Is(err, wantErr) || result.Rotated != wantRotated || result.FileWritten != wantWritten || openCalls != wantOpen {
				t.Fatalf("Record() = (%+v, %v), opens %d, want rotated=%t/written=%t/error %v/opens %d", result, err, openCalls, wantRotated, wantWritten, wantErr, wantOpen)
			}
		})
	}
}

func TestLogger_ConcurrentCapacityRotation(t *testing.T) {
	now := fixedEntryTime()
	logger, files, previous, _, _ := newWritableLogger(t, func() time.Time { return now })
	previous.write = func([]byte) (int, error) { return 0, filesystem.ErrRuntimeLogCapacity }
	nextPath, err := logger.layout.RuntimeLogSegmentFile("doctor", now, 1)
	if err != nil {
		t.Fatal(err)
	}
	var content bytes.Buffer
	next := fullFakeWriter(nextPath)
	next.write = content.Write
	opens := 0
	files.openNext = func(context.Context, string, time.Time) (logWriter, error) {
		opens++
		return next, nil
	}
	const count = 32
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
			t.Fatal(err)
		}
	}
	if opens != 1 {
		t.Fatalf("OpenNext() calls = %d, want 1", opens)
	}
	found := make(map[string]int)
	decoder := json.NewDecoder(&content)
	for {
		var record struct{ Message string }
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		found[record.Message]++
	}
	for index := range count {
		if found[fmt.Sprintf("record-%d", index)] != 1 {
			t.Fatalf("record %d count = %d, want 1", index, found[fmt.Sprintf("record-%d", index)])
		}
	}
}

func TestParseRetainedFile_SegmentOrder(t *testing.T) {
	layout := mustTestLayout(t)
	now := fixedEntryTime()
	candidates := make([]retentionCandidate, 0, 4)
	for _, sequence := range []uint64{1000, 998, 1001, 999} {
		path, err := layout.RuntimeLogSegmentFile("doctor", now, sequence)
		if err != nil {
			t.Fatal(err)
		}
		candidate, ok := parseRetainedFile(layout, fakeRetainedFile{name: filepath.Base(path), path: path}, now.Location())
		if !ok {
			t.Fatalf("parseRetainedFile(%q) rejected valid segment", path)
		}
		candidates = append(candidates, candidate)
	}
	active, err := layout.RuntimeLogSegmentFile("doctor", now, 1001)
	if err != nil {
		t.Fatal(err)
	}
	removed := selectRetentionRemovals(candidates, active, now, RetentionPolicy{MaxAgeDays: 30, MaxFilesPerCommand: 2})
	if len(removed) != 2 || removed[0].Path() != candidates[1].file.Path() || removed[1].Path() != candidates[3].file.Path() {
		t.Fatalf("removed = %v, want numeric sequence 998 and 999", namesOf(removed))
	}
	for _, suffix := range []string{".000.log", ".01.log", ".0001.log", ".-01.log", ".18446744073709551616.log"} {
		name := "doctor-" + now.Format("20060102") + suffix
		if _, ok := parseRetainedFile(layout, fakeRetainedFile{name: name, path: filepath.Join(layout.RuntimeLogDir(), name)}, now.Location()); ok {
			t.Fatalf("parseRetainedFile(%q) accepted malformed segment", name)
		}
	}
}

func TestLogger_OversizedEntryDoesNotCreateSegments(t *testing.T) {
	now := fixedEntryTime()
	logger, files, writer, _, _ := newWritableLogger(t, func() time.Time { return now })
	calls := 0
	writer.write = func([]byte) (int, error) {
		calls++
		return 0, filesystem.ErrRuntimeLogCapacity
	}
	files.openNext = func(context.Context, string, time.Time) (logWriter, error) {
		calls++
		return nil, errors.New("unexpected segment creation")
	}
	written, rotated, err := logger.appendFileLocked(t.Context(), now, make([]byte, filesystem.MaxRuntimeLogBytes+1))
	if written || rotated || !errors.Is(err, filesystem.ErrRuntimeLogCapacity) || calls != 0 {
		t.Fatalf("appendFileLocked() = %t/%t/%v, calls %d, want rejected before any write/create", written, rotated, err, calls)
	}
}
