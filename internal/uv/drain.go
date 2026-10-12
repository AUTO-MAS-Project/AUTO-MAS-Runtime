package uv

import (
	"errors"
	"os"
	"time"
)

// awaitUVStreams 保留正常 EOF 排空机会，脱离者持有写端时在预算内收口。
func awaitUVStreams(stdout, stderr *os.File, out, errs <-chan streamResult, budget time.Duration, cancel func()) (streamResult, streamResult, error) {
	timer := time.NewTimer(budget)
	defer timer.Stop()
	var a, b streamResult
	for out != nil || errs != nil {
		select {
		case a = <-out:
			out = nil
		case b = <-errs:
			errs = nil
		case <-timer.C:
			cancel()
			// 自持有的读端 Close 会中断 Windows 管道读；回调按 LineFunc 契约响应取消。
			closeErr := errors.Join(stdout.Close(), stderr.Close())
			if out != nil {
				a = <-out
			}
			if errs != nil {
				b = <-errs
			}
			return a, b, errors.Join(errors.New("uv output drain budget exceeded"), closeErr)
		}
	}
	return a, b, nil
}
