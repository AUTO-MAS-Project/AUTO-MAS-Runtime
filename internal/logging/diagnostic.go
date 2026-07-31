package logging

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
)

// WriteResult 精确描述一次调用已经对哪些 sink 产生字节副作用。
type WriteResult struct {
	FileWritten   bool
	StderrWritten bool
	Rotated       bool
}

func formatDiagnostic(
	now time.Time,
	level Level,
	command string,
	operationID string,
	message string,
	detailsJSON json.RawMessage,
) []byte {
	var line strings.Builder
	line.WriteString(now.Format(time.RFC3339Nano))
	line.WriteByte(' ')
	line.WriteString(strings.ToUpper(level.String()))
	line.WriteString(" [")
	line.WriteString(escapeVisible(command))
	line.WriteString("] [")
	line.WriteString(escapeVisible(operationID))
	line.WriteString("] ")
	line.WriteString(escapeVisible(message))
	line.WriteByte(' ')
	line.WriteString(string(detailsJSON))
	line.WriteByte('\n')
	return []byte(line.String())
}

func escapeVisible(value string) string {
	var escaped strings.Builder
	for _, character := range value {
		switch character {
		case '\r':
			escaped.WriteString(`\r`)
		case '\n':
			escaped.WriteString(`\n`)
		case '\t':
			escaped.WriteString(`\t`)
		default:
			if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
				appendUnicodeEscape(&escaped, character)
				continue
			}
			escaped.WriteRune(character)
		}
	}
	return escaped.String()
}

func appendUnicodeEscape(output *strings.Builder, character rune) {
	const hexadecimal = "0123456789ABCDEF"
	width := 4
	if character > 0xffff {
		width = 8
		output.WriteString(`\U`)
	} else {
		output.WriteString(`\u`)
	}
	for shift := (width - 1) * 4; shift >= 0; shift -= 4 {
		output.WriteByte(hexadecimal[(character>>shift)&0xf])
	}
}

func writeDiagnosticSinks(
	file io.Writer,
	stderr io.Writer,
	fileLine []byte,
	stderrLine []byte,
) (WriteResult, error) {
	fileWritten, fileErr := writeLine(file, fileLine)
	stderrWritten, stderrErr := writeLine(stderr, stderrLine)
	if fileErr != nil {
		fileErr = fmt.Errorf("write diagnostic file: %w", fileErr)
	}
	if stderrErr != nil {
		stderrErr = fmt.Errorf("write diagnostic stderr: %w", stderrErr)
	}
	return WriteResult{
		FileWritten:   fileWritten,
		StderrWritten: stderrWritten,
	}, errors.Join(fileErr, stderrErr)
}
