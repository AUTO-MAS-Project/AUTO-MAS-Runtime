package protocol

import (
	"fmt"
	"reflect"
)

// EventRenderer 在 Common 字段填充后投影协议事件。
// 渲染发生在 ProcessOutput 的串行化锁内，因此实现不得改变业务语义，
// 也不得回调当前 Emitter 或 ProcessOutput。
type EventRenderer interface {
	RenderHello(HelloEvent) error
	RenderProgress(ProgressEvent) error
	RenderState(StateEvent) error
	RenderLog(LogEvent) error
	RenderWarning(WarningEvent) error
	RenderError(ErrorEvent) error
	RenderResult(ResultEvent) error
}

// nonStickyRenderError 表示目标输出尚未受损，不能把该错误固化为后续写入错误。
type nonStickyRenderError struct {
	err error
}

func (e *nonStickyRenderError) Error() string { return e.err.Error() }

func (e *nonStickyRenderError) Unwrap() error { return e.err }

func renderEvent(renderer EventRenderer, event protocolEvent) error {
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

// interfaceIsNil 同时识别 interface 本身为 nil 和其中包裹的 typed nil。
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
