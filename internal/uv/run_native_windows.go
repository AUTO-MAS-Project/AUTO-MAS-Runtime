package uv

import "context"

type nativeRunResult struct {
	result UVResult
	err    error
}

func (*UVRunner) runNative(context.Context, []string, RunOptions, resolvedRunOptions) (nativeRunResult, bool) {
	return nativeRunResult{}, false
}
