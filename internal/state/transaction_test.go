package state

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/config"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestStore_NewStoreRejectsInvalidDependenciesBeforeIO(t *testing.T) {
	t.Parallel()

	layout := mustTestLayout(t)
	openCalls := 0
	validDependencies := storeDependencies{
		openFiles: func(context.Context, *config.Layout) (stateFiles, error) {
			openCalls++
			return completeFakeStateFiles(), nil
		},
		marshalIndent: json.MarshalIndent,
	}
	tests := []struct {
		name         string
		ctx          context.Context
		layout       *config.Layout
		dependencies storeDependencies
		options      []Option
	}{
		{name: "nil_context", layout: layout, dependencies: validDependencies},
		{name: "nil_layout", ctx: t.Context(), dependencies: validDependencies},
		{
			name:         "nil_option",
			ctx:          t.Context(),
			layout:       layout,
			dependencies: validDependencies,
			options:      []Option{nil},
		},
		{
			name:         "nil_clock",
			ctx:          t.Context(),
			layout:       layout,
			dependencies: validDependencies,
			options:      []Option{WithClock(nil)},
		},
		{
			name:         "duplicate_clock",
			ctx:          t.Context(),
			layout:       layout,
			dependencies: validDependencies,
			options: []Option{
				WithClock(time.Now),
				WithClock(time.Now),
			},
		},
		{
			name:         "nil_files_factory",
			ctx:          t.Context(),
			layout:       layout,
			dependencies: storeDependencies{marshalIndent: json.MarshalIndent},
		},
		{
			name:   "nil_marshaller",
			ctx:    t.Context(),
			layout: layout,
			dependencies: storeDependencies{
				openFiles: validDependencies.openFiles,
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			before := openCalls
			_, err := newStoreWithDependencies(
				test.ctx,
				test.layout,
				test.dependencies,
				test.options...,
			)
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("newStoreWithDependencies() error = %v, want *ValidationError", err)
			}
			if openCalls != before {
				t.Fatalf("openFiles calls = %d, want %d", openCalls, before)
			}
		})
	}
}

func TestStore_NewTransactionUsesClockOnceAndNormalizesUTC(t *testing.T) {
	t.Parallel()

	now := fixedStateTime()
	clockCalls := 0
	store := newTestStore(t, completeFakeStateFiles(), func() time.Time {
		clockCalls++
		return now
	})
	got, err := store.NewTransaction(
		TransactionMutation,
		validTransactionInput(TransactionMutation),
	)
	if err != nil {
		t.Fatalf("NewTransaction() error = %v", err)
	}
	if clockCalls != 1 {
		t.Fatalf("clock calls = %d, want 1", clockCalls)
	}
	if got.StartedAt.Location() != time.UTC || !got.StartedAt.Equal(now) {
		t.Fatalf("StartedAt = %v, want %v in UTC", got.StartedAt, now)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", got.SchemaVersion, SchemaVersion)
	}
}

func TestValidateTransaction_CommandStageMatrix(t *testing.T) {
	t.Parallel()

	allStages := protocol.AllStages()
	tests := []struct {
		name    string
		kind    TransactionKind
		command string
		target  string
		allowed map[protocol.Stage]bool
	}{
		{
			name: "backend_supervise", kind: TransactionBackend, command: "backend supervise",
			allowed: map[protocol.Stage]bool{
				protocol.StageBackendSpawn: true, protocol.StageBackendHealth: true,
				protocol.StageBackendRun: true, protocol.StageBackendRestart: true,
				protocol.StageBackendShutdown: true, protocol.StageBackendCleanup: true,
			},
		},
		{
			name: "mutation_bootstrap", kind: TransactionMutation, command: "bootstrap", target: "v5.4.0",
			allowed: map[protocol.Stage]bool{
				protocol.StageBootstrap: true,
				protocol.StageUVCheck:   true, protocol.StageUVDownload: true, protocol.StageUVVerify: true,
				protocol.StageWorkspaceCheck: true, protocol.StageWorkspaceClone: true,
				protocol.StageWorkspaceVerify: true, protocol.StageWorkspaceSwap: true,
				protocol.StageWorkspaceCleanup: true,
				protocol.StagePythonCheck:      true, protocol.StagePythonInstall: true,
				protocol.StageDependenciesCheck: true, protocol.StageDependenciesSync: true,
				protocol.StageDependenciesRebuild: true,
			},
		},
		{
			name: "mutation_workspace_sync", kind: TransactionMutation, command: "workspace sync", target: "v5.4.0",
			allowed: map[protocol.Stage]bool{
				protocol.StageWorkspaceCheck: true, protocol.StageWorkspaceClone: true,
				protocol.StageWorkspaceVerify: true, protocol.StageWorkspaceSwap: true,
				protocol.StageWorkspaceCleanup: true,
			},
		},
		{
			name: "mutation_environment_ensure", kind: TransactionMutation, command: "environment ensure",
			allowed: map[protocol.Stage]bool{
				protocol.StageUVCheck: true, protocol.StageUVDownload: true, protocol.StageUVVerify: true,
			},
		},
		{
			name: "mutation_environment_repair", kind: TransactionMutation, command: "environment repair",
			allowed: map[protocol.Stage]bool{
				protocol.StageUVCheck: true, protocol.StageUVDownload: true, protocol.StageUVVerify: true,
				protocol.StagePythonCheck: true, protocol.StagePythonInstall: true,
			},
		},
		{
			name: "mutation_dependencies_sync", kind: TransactionMutation, command: "dependencies sync",
			allowed: map[protocol.Stage]bool{
				protocol.StageDependenciesCheck: true, protocol.StageDependenciesSync: true,
			},
		},
		{
			name: "mutation_dependencies_rebuild", kind: TransactionMutation, command: "dependencies rebuild",
			allowed: map[protocol.Stage]bool{
				protocol.StageDependenciesCheck: true, protocol.StageDependenciesSync: true,
				protocol.StageDependenciesRebuild: true,
			},
		},
		{
			name: "mutation_repair", kind: TransactionMutation, command: "repair",
			allowed: map[protocol.Stage]bool{
				protocol.StageRepair:  true,
				protocol.StageUVCheck: true, protocol.StageUVDownload: true, protocol.StageUVVerify: true,
				protocol.StagePythonCheck: true, protocol.StagePythonInstall: true,
				protocol.StageDependenciesCheck: true, protocol.StageDependenciesSync: true,
				protocol.StageDependenciesRebuild: true,
			},
		},
		{
			name: "mutation_cleanup", kind: TransactionMutation, command: "cleanup",
			allowed: map[protocol.Stage]bool{
				protocol.StageCleanup: true, protocol.StageWorkspaceCleanup: true,
			},
		},
		{
			name: "update_bootstrap", kind: TransactionUpdate, command: "bootstrap", target: "v5.4.0",
			allowed: map[protocol.Stage]bool{
				protocol.StageWorkspaceClone: true, protocol.StageWorkspaceVerify: true,
				protocol.StageWorkspaceSwap: true, protocol.StageWorkspaceCleanup: true,
			},
		},
		{
			name: "update_workspace_sync", kind: TransactionUpdate, command: "workspace sync", target: "v5.4.0",
			allowed: map[protocol.Stage]bool{
				protocol.StageWorkspaceClone: true, protocol.StageWorkspaceVerify: true,
				protocol.StageWorkspaceSwap: true, protocol.StageWorkspaceCleanup: true,
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			for _, stage := range allStages {
				stage := stage
				t.Run(string(stage), func(t *testing.T) {
					value := validTransactionState(test.kind)
					value.Command = test.command
					value.Stage = stage
					value.TargetVersion = test.target
					err := ValidateTransaction(test.kind, value)
					wantAllowed := test.allowed[stage]
					if (err == nil) != wantAllowed {
						t.Fatalf(
							"ValidateTransaction(%q, %q) error = %v, want allowed %t",
							test.command,
							stage,
							err,
							wantAllowed,
						)
					}
				})
			}
		})
	}
}

func TestValidateTransaction_RejectsInvalidSemantics(t *testing.T) {
	t.Parallel()

	base := validTransactionState(TransactionMutation)
	tests := []struct {
		name   string
		kind   TransactionKind
		mutate func(*TransactionState)
		field  string
	}{
		{name: "unknown_kind", kind: TransactionKind("future"), field: "kind"},
		{name: "schema", kind: TransactionMutation, mutate: func(v *TransactionState) { v.SchemaVersion = 2 }, field: "schemaVersion"},
		{name: "operation", kind: TransactionMutation, mutate: func(v *TransactionState) { v.OperationID = "bad" }, field: "operationId"},
		{name: "command", kind: TransactionMutation, mutate: func(v *TransactionState) { v.Command = "backend supervise" }, field: "command"},
		{name: "pid", kind: TransactionMutation, mutate: func(v *TransactionState) { v.PID = 0 }, field: "pid"},
		{name: "time", kind: TransactionMutation, mutate: func(v *TransactionState) { v.StartedAt = time.Time{} }, field: "startedAt"},
		{name: "unknown_stage", kind: TransactionMutation, mutate: func(v *TransactionState) { v.Stage = protocol.Stage("future") }, field: "stage"},
		{name: "wrong_stage_domain", kind: TransactionMutation, mutate: func(v *TransactionState) { v.Stage = protocol.StageBackendRun }, field: "stage"},
		{name: "update_check", kind: TransactionUpdate, field: "stage"},
		{name: "required_target", kind: TransactionMutation, mutate: func(v *TransactionState) { v.TargetVersion = "" }, field: "targetVersion"},
		{name: "unsafe_target", kind: TransactionMutation, mutate: func(v *TransactionState) { v.TargetVersion = "v5..4" }, field: "targetVersion"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := base
			if test.mutate != nil {
				test.mutate(&value)
			}
			var validationErr *ValidationError
			err := ValidateTransaction(test.kind, value)
			if !errors.As(err, &validationErr) {
				t.Fatalf("ValidateTransaction() error = %v, want *ValidationError", err)
			}
			if validationErr.Field != test.field {
				t.Fatalf("ValidationError.Field = %q, want %q", validationErr.Field, test.field)
			}
		})
	}
}

func TestTransactionState_AlwaysMarshalsEveryField(t *testing.T) {
	t.Parallel()

	value := validTransactionState(TransactionBackend)
	value.TargetVersion = ""
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	want := []string{
		"schemaVersion",
		"operationId",
		"command",
		"pid",
		"startedAt",
		"targetVersion",
		"stage",
	}
	got := make([]string, 0, len(object))
	for _, key := range want {
		if _, ok := object[key]; ok {
			got = append(got, key)
		}
	}
	if !reflect.DeepEqual(got, want) || len(object) != len(want) {
		t.Fatalf("marshaled fields = %#v/%d, want %#v/%d", got, len(object), want, len(want))
	}
}
