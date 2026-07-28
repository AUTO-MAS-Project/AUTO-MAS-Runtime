// Package contracttest validates raw protocol NDJSON emitted by Runtime commands.
package contracttest

import "testing"

// Terminal identifies the expected operation outcome for a transcript.
type Terminal string

const (
	TerminalSuccess   Terminal = "success"
	TerminalFailure   Terminal = "failure"
	TerminalCancelled Terminal = "cancelled"
)

// Transcript contains the raw stdout bytes emitted by a command scenario.
type Transcript struct {
	Stdout []byte
}

// Runner executes one command scenario and returns its raw transcript.
type Runner func(t *testing.T, terminal Terminal) Transcript

// Register runs the common contract for every terminal scenario.
func Register(t *testing.T, command string, run Runner) {
	t.Helper()
	for _, terminal := range []Terminal{
		TerminalSuccess,
		TerminalFailure,
		TerminalCancelled,
	} {
		terminal := terminal
		t.Run(string(terminal), func(t *testing.T) {
			transcript := run(t, terminal)
			Assert(t, command, terminal, transcript.Stdout)
		})
	}
}

// Assert reports every contract violation in stdout.
func Assert(t *testing.T, command string, terminal Terminal, stdout []byte) {
	t.Helper()
	_, issues := inspect(command, terminal, stdout)
	for _, issue := range issues {
		t.Error(issue.Error())
	}
}
