package protocol

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
)

type humanDestination struct {
	writer      *bufio.Writer
	destination io.Writer
}

// HumanRenderer projects protocol events as deterministic human-readable text.
type HumanRenderer struct {
	stdout humanDestination
	stderr humanDestination
}

// NewHumanRenderer creates a human renderer that routes events to stdout and
// stderr according to the protocol's human-output contract.
func NewHumanRenderer(stdout, stderr io.Writer) (*HumanRenderer, error) {
	if interfaceIsNil(stdout) {
		return nil, errors.New("human stdout must not be nil")
	}
	if interfaceIsNil(stderr) {
		return nil, errors.New("human stderr must not be nil")
	}
	return &HumanRenderer{
		stdout: humanDestination{writer: bufio.NewWriter(stdout), destination: stdout},
		stderr: humanDestination{writer: bufio.NewWriter(stderr), destination: stderr},
	}, nil
}

// RenderHello renders a hello event to stdout.
func (r *HumanRenderer) RenderHello(event HelloEvent) error {
	var builder strings.Builder
	builder.WriteString("HELLO runtime=")
	builder.WriteString(humanScalar(event.RuntimeVersion))
	builder.WriteString(" command=")
	builder.WriteString(humanScalar(event.Command))
	builder.WriteString(" capabilities=")
	builder.WriteString(humanList(event.Capabilities))
	builder.WriteByte('\n')
	return r.stdout.write(builder.String())
}

// RenderProgress renders a progress event to stdout.
func (r *HumanRenderer) RenderProgress(event ProgressEvent) error {
	var builder strings.Builder
	builder.WriteString("PROGRESS [")
	builder.WriteString(humanScalar(string(event.Stage)))
	builder.WriteString("] ")
	builder.WriteString(humanScalar(string(event.Status)))
	if event.Current != nil {
		builder.WriteString(" current=")
		builder.WriteString(strconv.FormatInt(*event.Current, 10))
	}
	if event.Total != nil {
		builder.WriteString(" total=")
		builder.WriteString(strconv.FormatInt(*event.Total, 10))
	}
	if event.Percent != nil {
		builder.WriteString(" percent=")
		builder.WriteString(strconv.FormatFloat(*event.Percent, 'f', -1, 64))
		builder.WriteByte('%')
	}
	prefix := builder.String()
	appendHumanMessage(&builder, prefix, event.Message)
	return r.stdout.write(builder.String())
}

// RenderState renders a state event to stdout.
func (r *HumanRenderer) RenderState(event StateEvent) error {
	prefix := "STATE [" + humanScalar(string(event.Stage)) + "] " + humanScalar(string(event.Status))
	return r.stdout.write(humanMessageBlock(prefix, event.Message))
}

// RenderLog renders a log event to stdout only for the stdout stream.
func (r *HumanRenderer) RenderLog(event LogEvent) error {
	prefix := "LOG [" + humanScalar(event.Source) + ":" + humanScalar(event.Stream) + "]"
	destination := &r.stderr
	if event.Stream == "stdout" {
		destination = &r.stdout
	}
	return destination.write(humanMessageBlock(prefix, event.Message))
}

// RenderWarning renders a warning event to stderr.
func (r *HumanRenderer) RenderWarning(event WarningEvent) error {
	prefix := "WARNING [" + humanScalar(string(event.Stage)) + "] " + humanScalar(event.Code) +
		" retryable=" + strconv.FormatBool(event.Retryable) + " remediation=" + humanList(event.Remediation)
	return r.stderr.write(humanMessageBlock(prefix, event.Message))
}

// RenderError renders an error event to stderr.
func (r *HumanRenderer) RenderError(event ErrorEvent) error {
	prefix := "ERROR [" + humanScalar(string(event.Stage)) + "] " + humanScalar(event.Code) +
		" retryable=" + strconv.FormatBool(event.Retryable) + " remediation=" + humanList(event.Remediation)
	return r.stderr.write(humanMessageBlock(prefix, event.Message))
}

// RenderResult renders a successful result to stdout and a failed result to stderr.
func (r *HumanRenderer) RenderResult(event ResultEvent) error {
	prefix := "RESULT success=" + strconv.FormatBool(event.Success) +
		" code=" + humanScalar(event.Code) +
		" stage=" + humanScalar(string(event.Stage)) +
		" status=" + humanScalar(event.Status) +
		" retryable=" + strconv.FormatBool(event.Retryable) +
		" remediation=" + humanList(event.Remediation)
	destination := &r.stderr
	if event.Success {
		destination = &r.stdout
	}
	return destination.write(humanMessageBlock(prefix, event.Message))
}

func humanScalar(value string) string {
	if value == "" {
		return "-"
	}
	return escapeHumanText(strings.ToValidUTF8(value, "\uFFFD"))
}

func humanList(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	escaped := make([]string, len(values))
	for index, value := range values {
		escaped[index] = humanScalar(value)
	}
	return strings.Join(escaped, ",")
}

func humanMessageLines(message string) []string {
	normalized := strings.ToValidUTF8(message, "\uFFFD")
	normalized = strings.ReplaceAll(normalized, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	lines := strings.Split(normalized, "\n")
	for index, line := range lines {
		lines[index] = escapeHumanText(line)
	}
	return lines
}

func appendHumanMessage(builder *strings.Builder, prefix, message string) {
	for index, line := range humanMessageLines(message) {
		if index == 0 {
			builder.WriteString(" — ")
		} else {
			builder.WriteByte('\n')
			builder.WriteString(prefix)
			builder.WriteString(" | ")
		}
		builder.WriteString(line)
	}
	builder.WriteByte('\n')
}

func humanMessageBlock(prefix, message string) string {
	var builder strings.Builder
	builder.WriteString(prefix)
	appendHumanMessage(&builder, prefix, message)
	return builder.String()
}

func escapeHumanText(value string) string {
	var builder strings.Builder
	for _, runeValue := range value {
		switch runeValue {
		case '\\':
			builder.WriteString("\\\\")
		case '\r':
			builder.WriteString("\\r")
		case '\n':
			builder.WriteString("\\n")
		default:
			if unicode.IsControl(runeValue) {
				if runeValue <= 0xff {
					fmt.Fprintf(&builder, "\\x%02x", runeValue)
				} else {
					fmt.Fprintf(&builder, "\\u%04x", runeValue)
				}
				continue
			}
			builder.WriteRune(runeValue)
		}
	}
	return builder.String()
}

func (d *humanDestination) write(block string) error {
	if _, err := d.writer.WriteString(block); err != nil {
		return err
	}
	if err := d.writer.Flush(); err != nil {
		return err
	}
	return flushDestination(d.destination)
}
