//go:build linux || darwin

package backend

import (
	"os"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/process"
)

func TestMain(m *testing.M) {
	if handled, code := process.RunInternalGuardian(); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}
