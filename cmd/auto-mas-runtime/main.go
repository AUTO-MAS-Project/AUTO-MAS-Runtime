// Command auto-mas-runtime 管理本机 AUTO-MAS 运行时。
package main

import (
	"fmt"
	"os"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/cli"
)

func main() {
	// 进程入口是唯一持有 os.Stdout 和 os.Stderr 的位置。
	output, err := cli.Run()
	if err == nil {
		_, err = fmt.Fprintln(os.Stdout, output)
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
