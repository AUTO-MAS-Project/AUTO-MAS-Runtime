package cli

import (
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestPlatformCommandError(t *testing.T) {
	for _, goos := range []string{"windows", "darwin", "linux"} {
		for _, command := range []string{"version", "doctor", "bootstrap", "workspace check", "workspace sync", "workspace stage", "environment check", "environment prepare", "dependencies check", "dependencies sync", "dependencies rebuild", "backend supervise", "repair", "cleanup"} {
			t.Run(goos+"/"+command, func(t *testing.T) {
				err := platformCommandError(goos, command, protocol.StageBootstrap)
				if goos == "windows" || command == "version" {
					if err != nil {
						t.Fatalf("got %v, want nil", err)
					}
					return
				}
				failure, ok := err.(operationError)
				if !ok || failure.Code() != protocol.CodeUnsupportedMode || failure.Stage() != protocol.StageBootstrap {
					t.Fatalf("got %v, want UNSUPPORTED_MODE at bootstrap", err)
				}
			})
		}
	}
}
