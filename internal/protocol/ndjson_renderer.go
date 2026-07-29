package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// NDJSONRenderer 把每个协议事件渲染为一个以换行符结束的 JSON object。
type NDJSONRenderer struct {
	writer      *bufio.Writer
	destination io.Writer
}

// NewNDJSONRenderer 为 output 创建 NDJSON renderer。
func NewNDJSONRenderer(output io.Writer) (*NDJSONRenderer, error) {
	if interfaceIsNil(output) {
		return nil, errors.New("protocol output must not be nil")
	}
	return &NDJSONRenderer{
		writer:      bufio.NewWriter(output),
		destination: output,
	}, nil
}

// RenderHello 渲染 hello 事件。
func (r *NDJSONRenderer) RenderHello(event HelloEvent) error {
	return r.render(TypeHello, event)
}

// RenderProgress 渲染 progress 事件。
func (r *NDJSONRenderer) RenderProgress(event ProgressEvent) error {
	return r.render(TypeProgress, event)
}

// RenderState 渲染 state 事件。
func (r *NDJSONRenderer) RenderState(event StateEvent) error {
	return r.render(TypeState, event)
}

// RenderLog 渲染 log 事件。
func (r *NDJSONRenderer) RenderLog(event LogEvent) error {
	return r.render(TypeLog, event)
}

// RenderWarning 渲染 warning 事件。
func (r *NDJSONRenderer) RenderWarning(event WarningEvent) error {
	return r.render(TypeWarning, event)
}

// RenderError 渲染 error 事件。
func (r *NDJSONRenderer) RenderError(event ErrorEvent) error {
	return r.render(TypeError, event)
}

// RenderResult 渲染 result 事件。
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
