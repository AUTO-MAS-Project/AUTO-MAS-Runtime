// Command auto-mas-runtime 管理本机 AUTO-MAS 运行时。
package main

import (
	"context"
	"os"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/cli"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
)

func main() {
	if handled, code := process.RunInternalGuardian(); handled {
		os.Exit(code)
	}
	// 进程入口是唯一持有 os.Stdout 和 os.Stderr 的位置。
	os.Exit(cli.Execute(
		context.Background(),
		os.Args[1:],
		cli.IO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr},
	))
}
