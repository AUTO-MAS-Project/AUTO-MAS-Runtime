package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// NDJSONRenderer renders protocol events as one newline-delimited JSON object
// per event.
type NDJSONRenderer struct {
	writer      *bufio.Writer
	destination io.Writer
}

// NewNDJSONRenderer creates an NDJSON renderer for output.
func NewNDJSONRenderer(output io.Writer) (*NDJSONRenderer, error) {
	if interfaceIsNil(output) {
		return nil, errors.New("protocol output must not be nil")
	}
	return &NDJSONRenderer{
		writer:      bufio.NewWriter(output),
		destination: output,
	}, nil
}

// RenderHello renders a hello event.
func (r *NDJSONRenderer) RenderHello(event HelloEvent) error {
	return r.render(TypeHello, event)
}

// RenderProgress renders a progress event.
func (r *NDJSONRenderer) RenderProgress(event ProgressEvent) error {
	return r.render(TypeProgress, event)
}

// RenderState renders a state event.
func (r *NDJSONRenderer) RenderState(event StateEvent) error {
	return r.render(TypeState, event)
}

// RenderLog renders a log event.
func (r *NDJSONRenderer) RenderLog(event LogEvent) error {
	return r.render(TypeLog, event)
}

// RenderWarning renders a warning event.
func (r *NDJSONRenderer) RenderWarning(event WarningEvent) error {
	return r.render(TypeWarning, event)
}

// RenderError renders an error event.
func (r *NDJSONRenderer) RenderError(event ErrorEvent) error {
	return r.render(TypeError, event)
}

// RenderResult renders a result event.
func (r *NDJSONRenderer) RenderResult(event ResultEvent) error {
	return r.render(TypeResult, event)
}

func (r *NDJSONRenderer) render(eventType EventType, event any) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return &nonStickyRenderError{err: fmt.Errorf("encode %s event: %w", eventType, err)}
	}
	encoded = append(encoded, '\n')
	if _, err := r.writer.Write(encoded); err != nil {
		return err
	}
	if err := r.writer.Flush(); err != nil {
		return err
	}
	return flushDestination(r.destination)
}

type errorFlusher interface {
	Flush() error
}

type flusher interface {
	Flush()
}

func flushDestination(destination io.Writer) error {
	switch value := destination.(type) {
	case errorFlusher:
		return value.Flush()
	case flusher:
		value.Flush()
	}
	return nil
}
