package process

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	maxStreamFragmentBytes = 64 << 10
	maxStreamEventBytes    = 1 << 20
	streamTruncatedMarker  = "...[truncated]"
)

type streamLine struct {
	id            uint64
	originalBytes int64
	pending       string
	event         strings.Builder
	truncated     bool
}

func drainStream(
	ctx context.Context,
	stream string,
	reader io.Reader,
	sink StreamSink,
	recordError func(error),
) {
	buffered := bufio.NewReaderSize(reader, maxStreamFragmentBytes)
	line := streamLine{id: 1}
	var carry []byte
	for {
		fragment, readErr := buffered.ReadSlice('\n')
		endOfLine := len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
		if endOfLine {
			fragment = fragment[:len(fragment)-1]
		}
		line.originalBytes += int64(len(fragment))
		text, nextCarry := normalizeUTF8(append(carry, fragment...), endOfLine || errors.Is(readErr, io.EOF))
		carry = nextCarry
		line.append(ctx, stream, text, sink, recordError)
		if endOfLine {
			line.finish(ctx, stream, true, sink, recordError)
			line = streamLine{id: line.id + 1}
		}
		if readErr == nil || errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			if len(carry) > 0 {
				text, _ = normalizeUTF8(carry, true)
				line.append(ctx, stream, text, sink, recordError)
			}
			if line.originalBytes > 0 || line.pending != "" {
				line.finish(ctx, stream, false, sink, recordError)
			}
			return
		}
		if len(carry) > 0 {
			text, _ = normalizeUTF8(carry, true)
			line.append(ctx, stream, text, sink, recordError)
		}
		if line.originalBytes > 0 || line.pending != "" {
			line.finish(ctx, stream, false, sink, recordError)
		}
		recordError(fmtStreamError(stream, readErr))
		return
	}
}

func (l *streamLine) append(
	ctx context.Context,
	stream string,
	text string,
	sink StreamSink,
	recordError func(error),
) {
	if text == "" {
		return
	}
	if l.event.Len() < maxStreamEventBytes {
		remaining := maxStreamEventBytes - l.event.Len()
		prefix := truncateUTF8(text, remaining)
		_, _ = l.event.WriteString(prefix)
		if len(prefix) < len(text) {
			l.truncated = true
		}
	} else {
		l.truncated = true
	}
	for text != "" {
		fragment := truncateUTF8(text, maxStreamFragmentBytes)
		if fragment == "" {
			fragment = string(utf8.RuneError)
			_, size := utf8.DecodeRuneInString(text)
			text = text[size:]
		} else {
			text = text[len(fragment):]
		}
		if l.pending != "" {
			emitStreamRecord(ctx, sink, StreamRecord{
				Stream:   stream,
				LineID:   l.id,
				Fragment: l.pending,
			}, recordError)
		}
		l.pending = fragment
	}
}

func (l *streamLine) finish(
	ctx context.Context,
	stream string,
	endOfLine bool,
	sink StreamSink,
	recordError func(error),
) {
	if endOfLine && strings.HasSuffix(l.pending, "\r") {
		l.pending = strings.TrimSuffix(l.pending, "\r")
		l.originalBytes--
		value := l.event.String()
		l.event.Reset()
		_, _ = l.event.WriteString(strings.TrimSuffix(value, "\r"))
	}
	event := l.event.String()
	if l.truncated {
		limit := maxStreamEventBytes - len(streamTruncatedMarker)
		event = truncateUTF8(event, limit) + streamTruncatedMarker
	}
	emitStreamRecord(ctx, sink, StreamRecord{
		Stream:        stream,
		LineID:        l.id,
		Fragment:      l.pending,
		EndOfLine:     endOfLine,
		Event:         event,
		Truncated:     l.truncated,
		OriginalBytes: l.originalBytes,
	}, recordError)
}

func emitStreamRecord(ctx context.Context, sink StreamSink, record StreamRecord, recordError func(error)) {
	if sink == nil {
		return
	}
	if err := sink(ctx, record); err != nil {
		recordError(err)
	}
}

func normalizeUTF8(input []byte, final bool) (string, []byte) {
	var output strings.Builder
	for len(input) > 0 {
		if !utf8.FullRune(input) && !final {
			return output.String(), bytes.Clone(input)
		}
		runeValue, size := utf8.DecodeRune(input)
		if runeValue == utf8.RuneError && size == 1 {
			output.WriteRune(utf8.RuneError)
			input = input[1:]
			continue
		}
		output.WriteRune(runeValue)
		input = input[size:]
	}
	return output.String(), nil
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit <= 0 {
		return ""
	}
	prefix := value[:limit]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix
}

func fmtStreamError(stream string, err error) error {
	return fmt.Errorf("read managed process %s: %w", stream, err)
}
