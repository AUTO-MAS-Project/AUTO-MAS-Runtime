package telemetry

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/Microsoft/go-winio"
)

// StartupSampleInvitation 返回桌面为当前进程生成的本机管道邀请。
func StartupSampleInvitation() string {
	token := os.Getenv("AUTO_MAS_STARTUP_SAMPLE")
	if len(token) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(token); err != nil {
		return ""
	}
	return token
}

// readStartupResources 只在既有错误进入发送出口时向仍持有本次尝试的桌面取样。
func readStartupResources() map[string]any {
	token := StartupSampleInvitation()
	if token == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	conn, err := winio.DialPipeContext(ctx, `\\.\pipe\auto-mas-startup-`+token[:32])
	if err != nil {
		return nil
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil
	}
	if _, err := io.WriteString(conn, token[32:]+"\n"); err != nil {
		return nil
	}
	const limit = 32 * 1024
	body, err := io.ReadAll(io.LimitReader(conn, limit+1))
	if err != nil || len(body) > limit {
		return nil
	}
	var fields map[string]any
	if json.Unmarshal(body, &fields) != nil {
		return nil
	}
	return sanitizeStartupResources(fields)
}
