package cli

import (
	"bytes"
	"testing"
)

func TestRunPrintsDevelopmentVersion(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer

	if err := Run(&output); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	const want = "auto-mas-runtime dev\n"
	if got := output.String(); got != want {
		t.Fatalf("Run() output = %q, want %q", got, want)
	}
}
