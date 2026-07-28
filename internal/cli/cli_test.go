package cli

import (
	"testing"
)

func TestRunPrintsDevelopmentVersion(t *testing.T) {
	t.Parallel()

	output, err := Run()
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	const want = "auto-mas-runtime dev"
	if output != want {
		t.Fatalf("Run() output = %q, want %q", output, want)
	}
}
