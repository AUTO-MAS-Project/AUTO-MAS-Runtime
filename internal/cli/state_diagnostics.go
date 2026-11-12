package cli

import (
	"errors"
	"strings"
)

const (
	maxStateErrorChainItems = 32
	maxStateErrorTextBytes  = 4096
)

// localStateErrorChain 展开包装与并列错误，让本地日志保留被 Error 摘要隐藏的底层故障。
// 数量与文本大小有界，错误链中的路径和原始文本不进入遥测诊断。
func localStateErrorChain(err error) ([]string, bool) {
	values := make([]string, 0)
	pending := []error{err}
	truncated := false
	for len(pending) > 0 && len(values) < maxStateErrorChainItems {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == nil {
			continue
		}
		message := current.Error()
		if len(message) > maxStateErrorTextBytes {
			message = strings.ToValidUTF8(message[:maxStateErrorTextBytes], "")
			truncated = true
		}
		values = append(values, message)
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			for index := len(children) - 1; index >= 0; index-- {
				pending = append(pending, children[index])
			}
		} else if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			pending = append(pending, wrapped.Unwrap())
		}
	}
	return values, truncated || len(pending) > 0
}

func writeStateFailureDiagnostic(output IO, cause error) {
	values, truncated := localStateErrorChain(cause)
	for _, value := range values {
		writeDiagnostic(output, errors.New(value))
	}
	if truncated {
		writeDiagnostic(output, errors.New("state error chain truncated"))
	}
}
