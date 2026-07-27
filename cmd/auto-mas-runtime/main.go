// Command auto-mas-runtime manages the local AUTO-MAS runtime.
package main

import (
	"fmt"
	"os"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/cli"
)

func main() {
	if err := cli.Run(os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
