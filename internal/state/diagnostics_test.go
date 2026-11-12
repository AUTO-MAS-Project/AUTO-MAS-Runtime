package state

import (
	"errors"
	"reflect"
	"syscall"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

func TestStateErrors_DiagnosticDetails(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		kind string
	}{
		{name: "corrupt", err: &CorruptError{File: "mutation", Cause: errInvalidJSON}, kind: "corrupt"},
		{name: "schema", err: &UnsupportedSchemaError{File: "environment", Got: 9}, kind: "unsupported_schema"},
		{name: "validation", err: &ValidationError{Field: "pid", Cause: errInvalidValue}, kind: "validation"},
		{name: "pid", err: &PIDProbeError{Operation: "open-process", PID: 123, Cause: syscall.Errno(5)}, kind: "pid_probe"},
	} {
		t.Run(test.name, func(t *testing.T) {
			diagnostic, ok := test.err.(interface{ DiagnosticDetails() map[string]any })
			if !ok {
				t.Fatal("missing State diagnostic details")
			}
			details, _ := diagnostic.DiagnosticDetails()["persistence"].(map[string]any)
			if details["failureKind"] != test.kind {
				t.Fatalf("got %#v, want %s", details, test.kind)
			}
		})
	}
}

func TestWriteError_DiagnosticDetails(t *testing.T) {
	for _, test := range []struct {
		name string
		err  *WriteError
		want map[string]any
	}{
		{name: "committed finalize with cleanup failure", err: &WriteError{
			File: "mutation", Phase: WritePhaseFinalize, MutationApplied: true, RecoveryRequired: true,
			Cause:        &filesystem.FileError{Operation: "classic-unlink", Path: "private-path", Err: syscall.Errno(5)},
			CleanupError: &filesystem.FileError{Operation: "close", Path: "private-path", Err: syscall.Errno(6)},
		}, want: map[string]any{"persistence": map[string]any{
			"component": "mutation", "phase": "finalize", "mutationApplied": true, "recoveryRequired": true,
			"primary": map[string]any{"operation": "classic-unlink", "windowsError": uint64(5)},
			"cleanup": map[string]any{"operation": "close", "windowsError": uint64(6)},
		}}},
		{name: "recovery identity failure", err: &WriteError{
			File: "update", Phase: WritePhaseRecover, RecoveryRequired: true, Cause: filesystem.ErrIdentityChanged,
		}, want: map[string]any{"persistence": map[string]any{
			"component": "update", "phase": "recover", "mutationApplied": false, "recoveryRequired": true,
			"primary": map[string]any{"failureKind": "identity_changed"},
		}}},
		{name: "unknown file is omitted", err: &WriteError{File: "private-path", Phase: WritePhaseWrite, Cause: errors.New("private cause")},
			want: map[string]any{"persistence": map[string]any{"phase": "write", "mutationApplied": false, "recoveryRequired": false,
				"primary": map[string]any{"failureKind": "unknown"}}}},
		{name: "unknown cleanup is still a failure", err: &WriteError{File: "update", Phase: WritePhaseClose, CleanupError: errors.New("private cleanup")},
			want: map[string]any{"persistence": map[string]any{"component": "update", "phase": "close", "mutationApplied": false, "recoveryRequired": false,
				"cleanup": map[string]any{"failureKind": "unknown"}}}},
		{name: "errno belongs to its own operation", err: &WriteError{File: "mutation", Phase: WritePhaseFinalize,
			Cause: errors.Join(&filesystem.FileError{Operation: "verify-anchor", Path: "private-path", Err: filesystem.ErrIdentityChanged}, syscall.Errno(6)),
		}, want: map[string]any{"persistence": map[string]any{
			"component": "mutation", "phase": "finalize", "mutationApplied": false, "recoveryRequired": false,
			"primary": map[string]any{"operation": "verify-anchor", "failureKind": "identity_changed"},
		}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			diagnostic, ok := any(test.err).(interface{ DiagnosticDetails() map[string]any })
			if !ok {
				t.Fatal("WriteError has no diagnostic details")
			}
			if got := diagnostic.DiagnosticDetails(); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("diagnostics = %#v, want %#v", got, test.want)
			}
			if test.err.Cause != nil && !errors.Is(test.err, test.err.Cause) {
				t.Fatal("error chain no longer contains the primary cause")
			}
			if test.err.CleanupError != nil && !errors.Is(test.err, test.err.CleanupError) {
				t.Fatal("error chain no longer contains the cleanup cause")
			}
		})
	}
}

func TestReadError_DiagnosticDetails(t *testing.T) {
	err := &ReadError{File: "mutation", Cause: &filesystem.FileError{
		Operation: "open-read", Path: "private-path", Err: syscall.Errno(32),
	}}
	diagnostic, ok := any(err).(interface{ DiagnosticDetails() map[string]any })
	if !ok {
		t.Fatal("ReadError has no diagnostic details")
	}
	want := map[string]any{"persistence": map[string]any{
		"component": "mutation", "phase": "read",
		"primary": map[string]any{"operation": "open-read", "windowsError": uint64(32)},
	}}
	if got := diagnostic.DiagnosticDetails(); !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostics = %#v, want %#v", got, want)
	}
}
