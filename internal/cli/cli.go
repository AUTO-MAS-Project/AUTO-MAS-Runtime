// Package cli parses commands and delegates work to application services.
package cli

import (
	"fmt"
	"io"
)

// Run executes the runtime command.
func Run(output io.Writer) error {
	_, err := fmt.Fprintln(output, "auto-mas-runtime dev")
	return err
}
