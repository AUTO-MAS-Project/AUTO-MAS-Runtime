// Command release-smoke 验证下载资产的 NDJSON 身份与终态。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
)

func main() {
	if len(os.Args) != 3 {
		panic("expected binary and version")
	}
	root, err := os.MkdirTemp("", "runtime-smoke-")
	check(err)
	defer os.RemoveAll(root)
	commands := [][]string{{"version", "--output", "ndjson"}}
	if runtime.GOOS != "darwin" {
		commands = append(commands, []string{"doctor", "--output", "ndjson", "--app-root", root})
	}
	for _, args := range commands {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		cmd := exec.CommandContext(ctx, os.Args[1], args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		cancel()
		if err != nil {
			panic(fmt.Sprintf("%v: %v: %s", args, err, stderr.String()))
		}
		lines := bytes.Split(bytes.TrimSuffix(stdout.Bytes(), []byte("\n")), []byte("\n"))
		terminal := 0
		for i, line := range lines {
			var event struct {
				Type           string `json:"type"`
				Sequence       int    `json:"sequence"`
				RuntimeVersion string `json:"runtimeVersion"`
				Success        bool   `json:"success"`
			}
			check(json.Unmarshal(line, &event))
			if event.Sequence != i+1 {
				panic("invalid sequence")
			}
			if i == 0 && (event.Type != "hello" || event.RuntimeVersion != os.Args[2]) {
				panic("invalid hello")
			}
			if event.Type == "result" {
				terminal++
				if i != len(lines)-1 || !event.Success {
					panic("invalid terminal result")
				}
			}
		}
		if terminal != 1 {
			panic("missing or duplicate terminal result")
		}
	}
}
func check(err error) {
	if err != nil {
		panic(err)
	}
}
