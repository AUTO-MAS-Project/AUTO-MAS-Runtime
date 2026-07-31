package state

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestStateEnums_StringAndValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		got   interface{ String() string }
		want  string
		valid bool
	}{
		{name: "transaction_backend", got: TransactionBackend, want: "backend", valid: TransactionBackend.Valid()},
		{name: "transaction_mutation", got: TransactionMutation, want: "mutation", valid: TransactionMutation.Valid()},
		{name: "transaction_update", got: TransactionUpdate, want: "update", valid: TransactionUpdate.Valid()},
		{name: "reason_repository", got: ReasonRepositoryChanged, want: "repository_changed", valid: ReasonRepositoryChanged.Valid()},
		{name: "reason_operation", got: ReasonOperationFailed, want: "operation_failed", valid: ReasonOperationFailed.Valid()},
		{name: "phase_encode", got: WritePhaseEncode, want: "encode", valid: WritePhaseEncode.Valid()},
		{name: "phase_recover", got: WritePhaseRecover, want: "recover", valid: WritePhaseRecover.Valid()},
		{name: "phase_create", got: WritePhaseCreate, want: "create", valid: WritePhaseCreate.Valid()},
		{name: "phase_write", got: WritePhaseWrite, want: "write", valid: WritePhaseWrite.Valid()},
		{name: "phase_sync", got: WritePhaseSync, want: "sync", valid: WritePhaseSync.Valid()},
		{name: "phase_rename", got: WritePhaseRename, want: "rename", valid: WritePhaseRename.Valid()},
		{name: "phase_finalize", got: WritePhaseFinalize, want: "finalize", valid: WritePhaseFinalize.Valid()},
		{name: "phase_close", got: WritePhaseClose, want: "close", valid: WritePhaseClose.Valid()},
		{name: "phase_remove", got: WritePhaseRemove, want: "remove", valid: WritePhaseRemove.Valid()},
		{name: "mutex_backend", got: MutexBackend, want: "backend", valid: MutexBackend.Valid()},
		{name: "mutex_mutation", got: MutexMutation, want: "mutation", valid: MutexMutation.Valid()},
		{name: "activity_active", got: ActivityActive, want: "active", valid: ActivityActive.Valid()},
		{name: "activity_stale", got: ActivityStale, want: "stale", valid: ActivityStale.Valid()},
		{name: "activity_inconsistent", got: ActivityInconsistent, want: "inconsistent", valid: ActivityInconsistent.Valid()},
		{name: "activity_unknown", got: ActivityUnknown, want: "unknown", valid: ActivityUnknown.Valid()},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.got.String(); got != test.want {
				t.Fatalf("String() = %q, want %q", got, test.want)
			}
			if !test.valid {
				t.Fatalf("Valid() = false for %q, want true", test.want)
			}
		})
	}

	invalid := []struct {
		name  string
		got   string
		want  string
		valid bool
	}{
		{name: "transaction_zero", got: TransactionKind("").String(), want: "", valid: TransactionKind("").Valid()},
		{name: "transaction_unknown", got: TransactionKind("future").String(), want: "future", valid: TransactionKind("future").Valid()},
		{name: "reason_zero", got: BrokenReason("").String(), want: "", valid: BrokenReason("").Valid()},
		{name: "reason_unknown", got: BrokenReason("future").String(), want: "future", valid: BrokenReason("future").Valid()},
		{name: "phase_zero", got: WritePhase("").String(), want: "", valid: WritePhase("").Valid()},
		{name: "phase_unknown", got: WritePhase("future").String(), want: "future", valid: WritePhase("future").Valid()},
		{name: "mutex_zero", got: MutexKind("").String(), want: "", valid: MutexKind("").Valid()},
		{name: "mutex_unknown", got: MutexKind("future").String(), want: "future", valid: MutexKind("future").Valid()},
		{name: "activity_zero", got: Activity("").String(), want: "", valid: Activity("").Valid()},
		{name: "activity_unknown_value", got: Activity("future").String(), want: "future", valid: Activity("future").Valid()},
	}
	for _, test := range invalid {
		if test.valid {
			t.Fatalf("%s Valid() = true, want false", test.name)
		}
		if test.got != test.want {
			t.Fatalf("%s String() = %q, want %q", test.name, test.got, test.want)
		}
	}
}

func TestStateErrors_PreserveSentinelsAndMultipleCauses(t *testing.T) {
	t.Parallel()

	readCause := errors.New("read denied")
	cleanupCause := errors.New("cleanup denied")
	tests := []struct {
		name string
		err  error
		want error
	}{
		{
			name: "not_found",
			err:  &NotFoundError{File: "backend", Path: `D:\managed\backend.json`},
			want: ErrNotFound,
		},
		{
			name: "corrupt",
			err:  &CorruptError{File: "backend", Cause: readCause},
			want: ErrCorrupt,
		},
		{
			name: "unsupported",
			err:  &UnsupportedSchemaError{File: "environment", Got: 2},
			want: ErrUnsupportedSchema,
		},
		{
			name: "read_chain",
			err:  &ReadError{File: "mutation", Cause: readCause},
			want: readCause,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if !errors.Is(test.err, test.want) {
				t.Fatalf("errors.Is(%v, %v) = false, want true", test.err, test.want)
			}
		})
	}

	writeErr := &WriteError{
		File:             "update",
		Phase:            WritePhaseFinalize,
		MutationApplied:  true,
		RecoveryRequired: true,
		Cause:            readCause,
		CleanupError:     cleanupCause,
	}
	if !errors.Is(writeErr, readCause) || !errors.Is(writeErr, cleanupCause) {
		t.Fatalf("WriteError chains = %v, want both causes", writeErr)
	}
	if got := writeErr.Code(); got != protocol.CodeStateWriteFailed {
		t.Fatalf("WriteError.Code() = %q, want %q", got, protocol.CodeStateWriteFailed)
	}
	if !writeErr.MutationApplied || !writeErr.RecoveryRequired {
		t.Fatalf(
			"WriteError facts = %t/%t, want true/true",
			writeErr.MutationApplied,
			writeErr.RecoveryRequired,
		)
	}

	for _, err := range []error{
		&NotFoundError{File: "backend\nforged", Path: "path\rforged"},
		&CorruptError{File: "backend\nforged"},
		&ValidationError{Field: "field\nforged"},
		&UnsupportedSchemaError{File: "environment\nforged", Got: 2},
		&ReadError{File: "mutation\nforged"},
		&WriteError{File: "update\nforged", Phase: WritePhase("phase\rforged")},
		&PIDProbeError{Operation: "open\nforged", PID: 1},
	} {
		if strings.ContainsAny(err.Error(), "\r\n") {
			t.Fatalf("error contains raw control character: %q", err.Error())
		}
	}
}

func TestStateErrors_ErrorMethodsAreStableAndRedacted(t *testing.T) {
	t.Parallel()

	const sensitive = `{"secret":"must-not-leak"}`
	cause := fmt.Errorf("read payload: %s: %w", sensitive, fs.ErrPermission)
	tests := []struct {
		name   string
		first  error
		second error
		is     error
		unwrap error
	}{
		{
			name:   "not_found",
			first:  &NotFoundError{File: "backend", Path: `D:\managed\backend.json`},
			second: &NotFoundError{File: "backend", Path: `D:\managed\backend.json`},
			is:     ErrNotFound,
		},
		{
			name:   "corrupt",
			first:  &CorruptError{File: "mutation", Cause: cause},
			second: &CorruptError{File: "mutation", Cause: cause},
			is:     ErrCorrupt,
			unwrap: cause,
		},
		{
			name:   "unsupported",
			first:  &UnsupportedSchemaError{File: "environment", Got: 2},
			second: &UnsupportedSchemaError{File: "environment", Got: 2},
			is:     ErrUnsupportedSchema,
		},
		{
			name:   "read",
			first:  &ReadError{File: "update", Cause: cause},
			second: &ReadError{File: "update", Cause: cause},
			unwrap: cause,
		},
		{
			name:   "validation",
			first:  &ValidationError{Field: "operationId", Cause: cause},
			second: &ValidationError{Field: "operationId", Cause: cause},
			unwrap: cause,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := test.first.Error()
			if got == "" || got != test.second.Error() {
				t.Fatalf("Error() = %q/%q, want equal non-empty strings", got, test.second.Error())
			}
			if strings.Contains(got, sensitive) || strings.Contains(got, "must-not-leak") {
				t.Fatalf("Error() leaked raw JSON: %q", got)
			}
			if test.is != nil && !errors.Is(test.first, test.is) {
				t.Fatalf("errors.Is(%v, %v) = false, want true", test.first, test.is)
			}
			if test.unwrap != nil && !errors.Is(test.first, test.unwrap) {
				t.Fatalf("errors.Is(%v, unwrap) = false, want true", test.first)
			}
		})
	}
}

func TestSafeValidators_RejectUnsafeAndAcceptFrozenForms(t *testing.T) {
	t.Parallel()

	if err := validateOperationID("01ARZ3NDEKTSV4RRFFQ69G5FAV"); err != nil {
		t.Fatalf("validateOperationID(valid) error = %v", err)
	}
	if err := validateProductVersion("v5.4.0-beta.1"); err != nil {
		t.Fatalf("validateProductVersion(valid) error = %v", err)
	}
	if err := validateToolVersion("3.12.10"); err != nil {
		t.Fatalf("validateToolVersion(valid) error = %v", err)
	}
	if err := validateCommit("0123456789abcdef0123456789abcdef01234567"); err != nil {
		t.Fatalf("validateCommit(valid) error = %v", err)
	}
	if err := validateTimestamp("startedAt", time.Date(2026, 7, 29, 1, 2, 3, 4, time.UTC)); err != nil {
		t.Fatalf("validateTimestamp(valid) error = %v", err)
	}
	root := t.TempDir()
	layout, err := config.NewLayout(root, root)
	if err != nil {
		t.Fatalf("config.NewLayout() error = %v", err)
	}
	if err := validateRuntimeLogPath(
		layout,
		filepath.Join(layout.RuntimeLogDir(), "runtime.log"),
	); err != nil {
		t.Fatalf("validateRuntimeLogPath(valid) error = %v", err)
	}

	tests := []struct {
		name string
		call func() error
	}{
		{name: "lowercase_ulid", call: func() error { return validateOperationID("01arz3ndektsv4rrffq69g5fav") }},
		{name: "overflow_ulid", call: func() error { return validateOperationID("81ARZ3NDEKTSV4RRFFQ69G5FAV") }},
		{name: "product_parent", call: func() error { return validateProductVersion("v5..4") }},
		{name: "product_ref", call: func() error { return validateProductVersion("v5@{1}") }},
		{name: "product_trailing_dot", call: func() error { return validateProductVersion("v5.") }},
		{name: "product_too_long", call: func() error { return validateProductVersion("v" + strings.Repeat("a", 128)) }},
		{name: "tool_path", call: func() error { return validateToolVersion(`3.12\bin`) }},
		{name: "tool_c0_control", call: func() error { return validateToolVersion("3.12\n") }},
		{name: "tool_del_control", call: func() error { return validateToolVersion("3.12\u007f") }},
		{name: "tool_c1_control", call: func() error { return validateToolVersion("3.12\u0085") }},
		{name: "log_c0_control", call: func() error {
			return validateRuntimeLogPath(layout, filepath.Join(layout.RuntimeLogDir(), "run\n.log"))
		}},
		{name: "log_del_control", call: func() error {
			return validateRuntimeLogPath(layout, filepath.Join(layout.RuntimeLogDir(), "run\u007f.log"))
		}},
		{name: "log_c1_control", call: func() error {
			return validateRuntimeLogPath(layout, filepath.Join(layout.RuntimeLogDir(), "run\u0085.log"))
		}},
		{name: "tool_too_long", call: func() error { return validateToolVersion(strings.Repeat("a", 129)) }},
		{name: "uppercase_commit", call: func() error { return validateCommit(strings.Repeat("A", 40)) }},
		{name: "zero_time", call: func() error { return validateTimestamp("startedAt", time.Time{}) }},
		{name: "unrepresentable_time", call: func() error { return validateTimestamp("startedAt", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)) }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var validationErr *ValidationError
			if err := test.call(); !errors.As(err, &validationErr) {
				t.Fatalf("validator error = %v, want *ValidationError", err)
			}
		})
	}
}
