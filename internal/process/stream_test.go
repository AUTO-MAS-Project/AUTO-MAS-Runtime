package process

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestJob_PipeInterleavesStreamsAndFlushesTail(t *testing.T) {
	var records []StreamRecord
	var recordsMu sync.Mutex
	sink := func(_ context.Context, record StreamRecord) error {
		recordsMu.Lock()
		defer recordsMu.Unlock()
		records = append(records, record)
		return nil
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		drainStream(t.Context(), StreamStdout, strings.NewReader("out-one\nout-tail"), sink, func(err error) { t.Error(err) })
	}()
	go func() {
		defer readers.Done()
		drainStream(t.Context(), StreamStderr, strings.NewReader("err-one\nerr-tail\n"), sink, func(err error) { t.Error(err) })
	}()
	readers.Wait()

	byStream := map[string][]StreamRecord{}
	for _, record := range records {
		byStream[record.Stream] = append(byStream[record.Stream], record)
	}
	if got := recordEvents(byStream[StreamStdout]); strings.Join(got, "|") != "out-one|out-tail" {
		t.Fatalf("stdout events = %#v", got)
	}
	if got := recordEvents(byStream[StreamStderr]); strings.Join(got, "|") != "err-one|err-tail" {
		t.Fatalf("stderr events = %#v", got)
	}
	if byStream[StreamStdout][1].EndOfLine {
		t.Fatal("unterminated stdout tail was marked end-of-line")
	}
	if !byStream[StreamStderr][1].EndOfLine {
		t.Fatal("terminated stderr tail was not marked end-of-line")
	}

	t.Run("reader error still flushes tail", func(t *testing.T) {
		want := errors.New("reader injection")
		var tail []StreamRecord
		var readErr error
		drainStream(t.Context(), StreamStdout, &terminalErrorReader{payload: []byte("error-tail"), err: want}, func(_ context.Context, record StreamRecord) error {
			tail = append(tail, record)
			return nil
		}, func(err error) { readErr = err })
		if !errors.Is(readErr, want) || len(tail) != 1 || tail[0].Event != "error-tail" || tail[0].EndOfLine {
			t.Fatalf("records=%#v err=%v", tail, readErr)
		}
	})
}

type terminalErrorReader struct {
	payload []byte
	err     error
}

func (r *terminalErrorReader) Read(buffer []byte) (int, error) {
	if len(r.payload) == 0 {
		return 0, r.err
	}
	count := copy(buffer, r.payload)
	r.payload = r.payload[count:]
	return count, r.err
}

func TestJob_PipeReplacesInvalidUTF8AndBoundsEventLine(t *testing.T) {
	line := append([]byte{'o', 'k', '-'}, 0xff, 0xfe)
	line = append(line, bytes.Repeat([]byte("界"), maxStreamEventBytes)...)
	line = append(line, '\n')
	var records []StreamRecord
	drainStream(t.Context(), StreamStdout, bytes.NewReader(line), func(_ context.Context, record StreamRecord) error {
		records = append(records, record)
		return nil
	}, func(err error) { t.Error(err) })
	if len(records) < 2 {
		t.Fatalf("record count = %d, want fragmented output", len(records))
	}
	for _, record := range records {
		if len(record.Fragment) > maxStreamFragmentBytes || !utf8.ValidString(record.Fragment) {
			t.Fatalf("invalid fragment length=%d valid=%t", len(record.Fragment), utf8.ValidString(record.Fragment))
		}
	}
	last := records[len(records)-1]
	if !last.EndOfLine || !last.Truncated || len(last.Event) > maxStreamEventBytes ||
		!strings.HasSuffix(last.Event, streamTruncatedMarker) || !utf8.ValidString(last.Event) {
		t.Fatalf("last record = %#v", last)
	}
	if !strings.Contains(last.Event, string(utf8.RuneError)) {
		t.Fatal("event did not replace invalid UTF-8")
	}
	if last.OriginalBytes != int64(len(line)-1) {
		t.Fatalf("original bytes = %d, want %d", last.OriginalBytes, len(line)-1)
	}
}

func recordEvents(records []StreamRecord) []string {
	result := make([]string, 0, len(records))
	for _, record := range records {
		if record.Event != "" || record.EndOfLine {
			result = append(result, record.Event)
		}
	}
	return result
}
