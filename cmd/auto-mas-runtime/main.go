// Command auto-mas-runtime manages the local AUTO-MAS runtime.
package main

import (
	"fmt"
	"os"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/cli"
)

func main() {
	output, err := cli.Run()
	if err == nil {
		_, err = fmt.Fprintln(os.Stdout, output)
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
