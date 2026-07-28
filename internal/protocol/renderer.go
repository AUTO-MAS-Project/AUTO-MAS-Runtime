package protocol

import (
	"fmt"
	"reflect"
)

// EventRenderer projects protocol events after their Common fields have been
// populated. A renderer must not modify business semantics or call back into
// the current Emitter or ProcessOutput, because rendering occurs under its
// serialization lock.
type EventRenderer interface {
	RenderHello(HelloEvent) error
	RenderProgress(ProgressEvent) error
	RenderState(StateEvent) error
	RenderLog(LogEvent) error
	RenderWarning(WarningEvent) error
	RenderError(ErrorEvent) error
	RenderResult(ResultEvent) error
}

type nonStickyRenderError struct {
	err error
}

func (e *nonStickyRenderError) Error() string { return e.err.Error() }

func (e *nonStickyRenderError) Unwrap() error { return e.err }

func renderEvent(renderer EventRenderer, event eventWithCommon) error {
	switch value := event.(type) {
	case *HelloEvent:
		return renderer.RenderHello(*value)
	case *ProgressEvent:
		return renderer.RenderProgress(*value)
	case *StateEvent:
		return renderer.RenderState(*value)
	case *LogEvent:
		return renderer.RenderLog(*value)
	case *WarningEvent:
		return renderer.RenderWarning(*value)
	case *ErrorEvent:
		return renderer.RenderError(*value)
	case *ResultEvent:
		return renderer.RenderResult(*value)
	default:
		return &nonStickyRenderError{err: fmt.Errorf("render protocol event: unsupported event type")}
	}
}

func interfaceIsNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
