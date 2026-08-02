package cli

import (
	"context"
	"strings"
	"testing"
)

func TestExecute_HelpListsArchitectureCommandTree(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--help")
	if result.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.exitCode)
	}
	for _, want := range []string{
		"version",
		"doctor",
		"bootstrap",
		"workspace",
		"environment",
		"dependencies",
		"backend",
		"repair",
		"cleanup",
	} {
		if !strings.Contains(result.stdout, want) {
			t.Errorf("help stdout does not contain %q: %s", want, result.stdout)
		}
	}
	if strings.Contains(result.stdout, "completion") {
		t.Errorf("help stdout contains completion command: %s", result.stdout)
	}
	if strings.Contains(result.stdout, "help [command]") {
		t.Errorf("help stdout contains default help command: %s", result.stdout)
	}
}

func TestExecute_SubcommandHelpShowsOwnFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		command []string
		want    string
	}{
		{command: []string{"bootstrap", "--help"}, want: "--version"},
		{command: []string{"workspace", "sync", "--help"}, want: "--version"},
		{command: []string{"backend", "supervise", "--help"}, want: "supervise"},
	}
	for _, test := range tests {
		test := test
		t.Run(strings.Join(test.command, " "), func(t *testing.T) {
			t.Parallel()
			result := runCLI(t, context.Background(), test.command...)
			if result.exitCode != 0 {
				t.Fatalf("exit code = %d, want 0", result.exitCode)
			}
			if !strings.Contains(result.stdout, test.want) {
				t.Errorf("help stdout does not contain %q: %s", test.want, result.stdout)
			}
		})
	}
}

func TestExecute_NDJSONHelpGoesToStderr(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--output", "ndjson", "--help")
	if result.exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", result.exitCode)
	}
	if result.stdout != "" {
		t.Errorf("stdout = %q, want empty", result.stdout)
	}
	if !strings.Contains(result.stderr, "version") {
		t.Errorf("stderr = %q, want help text", result.stderr)
	}
}

func TestExecute_HelpCommandHiddenFromTree(t *testing.T) {
	t.Parallel()
	result := runCLI(t, context.Background(), "--help")
	if strings.Contains(result.stdout, "\n  help") {
		t.Errorf("help output lists help command: %s", result.stdout)
	}
}
