package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

const (
	testOldCommit = "89abcdef0123456789abcdef0123456789abcdef"
	testNewCommit = "0123456789abcdef0123456789abcdef01234567"
)

func validLastSuccessful() Revision {
	return Revision{Version: "v5.3.0", Commit: testOldCommit}
}

func validRepositoryChanged(store *Store) BrokenEnvironment {
	return BrokenEnvironment{
		TargetVersion: "v5.4.0-beta.1",
		Branch:        "release/v5.4.0-beta.1",
		Commit:        testNewCommit,
		Reason:        ReasonRepositoryChanged,
		Stage:         protocol.StageWorkspaceSwap,
		ExitCode:      0,
		LogPath:       filepath.Join(store.layout.RuntimeLogDir(), "workspace-sync-20260729.log"),
	}
}

func validOperationFailed(store *Store) BrokenEnvironment {
	return BrokenEnvironment{
		TargetVersion: "v5.4.0-beta.1",
		Branch:        "release/v5.4.0-beta.1",
		Commit:        testNewCommit,
		PythonVersion: "3.12.10",
		UVVersion:     "0.8.0",
		Reason:        ReasonOperationFailed,
		Stage:         protocol.StageDependenciesSync,
		ExitCode:      50,
		LogPath:       filepath.Join(store.layout.RuntimeLogDir(), "dependencies-sync-20260729.log"),
	}
}

func TestStore_EnvironmentConstructorsUseClockAndPreserveRevisions(t *testing.T) {
	t.Parallel()

	now := fixedStateTime()
	clockCalls := 0
	store := newTestStore(t, completeFakeStateFiles(), func() time.Time {
		clockCalls++
		return now
	})
	ready, err := store.NewReadyEnvironment("v5.3.0", testOldCommit)
	if err != nil {
		t.Fatalf("NewReadyEnvironment() error = %v", err)
	}
	if ready.Status != protocol.StateReadyToStart || ready.Broken != nil {
		t.Fatalf("ready state = %#v, want ready_to_start with nil broken", ready)
	}
	if ready.LastSuccessful != validLastSuccessful() {
		t.Fatalf("ready LastSuccessful = %#v, want %#v", ready.LastSuccessful, validLastSuccessful())
	}

	brokenInput := validRepositoryChanged(store)
	broken, err := store.NewBrokenEnvironment(validLastSuccessful(), brokenInput)
	if err != nil {
		t.Fatalf("NewBrokenEnvironment() error = %v", err)
	}
	if clockCalls != 2 {
		t.Fatalf("clock calls = %d, want 2", clockCalls)
	}
	if broken.Status != protocol.StateEnvironmentBroken || broken.Broken == nil {
		t.Fatalf("broken state = %#v, want environment_broken with details", broken)
	}
	if broken.LastSuccessful != validLastSuccessful() {
		t.Fatalf("broken LastSuccessful = %#v, want prior successful revision", broken.LastSuccessful)
	}
	if broken.Broken.Commit != testNewCommit || broken.Broken.Commit == broken.LastSuccessful.Commit {
		t.Fatalf(
			"active/last commits = %q/%q, want distinct target/prior revisions",
			broken.Broken.Commit,
			broken.LastSuccessful.Commit,
		)
	}
	brokenInput.Commit = testOldCommit
	if broken.Broken.Commit != testNewCommit {
		t.Fatal("NewBrokenEnvironment() retained caller alias, want defensive value copy")
	}
}

func TestValidateEnvironment_AllowsOnlyStableStatuses(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, completeFakeStateFiles(), fixedStateTime)
	ready, err := store.NewReadyEnvironment("v5.3.0", testOldCommit)
	if err != nil {
		t.Fatalf("NewReadyEnvironment() error = %v", err)
	}
	broken, err := store.NewBrokenEnvironment(
		validLastSuccessful(),
		validRepositoryChanged(store),
	)
	if err != nil {
		t.Fatalf("NewBrokenEnvironment() error = %v", err)
	}
	if err := store.ValidateEnvironment(ready); err != nil {
		t.Fatalf("ValidateEnvironment(ready) error = %v", err)
	}
	if err := store.ValidateEnvironment(broken); err != nil {
		t.Fatalf("ValidateEnvironment(broken) error = %v", err)
	}

	for _, status := range protocol.AllStateStatuses() {
		if status == protocol.StateReadyToStart ||
			status == protocol.StateEnvironmentBroken {
			continue
		}
		value := ready
		value.Status = status
		var validationErr *ValidationError
		if err := store.ValidateEnvironment(value); !errors.As(err, &validationErr) {
			t.Fatalf("ValidateEnvironment(status=%q) error = %v, want validation", status, err)
		}
	}
}

func TestValidateEnvironment_RejectsInconsistentFields(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, completeFakeStateFiles(), fixedStateTime)
	ready, err := store.NewReadyEnvironment("v5.3.0", testOldCommit)
	if err != nil {
		t.Fatalf("NewReadyEnvironment() error = %v", err)
	}
	broken, err := store.NewBrokenEnvironment(
		validLastSuccessful(),
		validOperationFailed(store),
	)
	if err != nil {
		t.Fatalf("NewBrokenEnvironment() error = %v", err)
	}
	tests := []struct {
		name   string
		base   EnvironmentState
		mutate func(*EnvironmentState)
		field  string
	}{
		{name: "schema", base: ready, mutate: func(v *EnvironmentState) { v.SchemaVersion = 2 }, field: "schemaVersion"},
		{name: "updated", base: ready, mutate: func(v *EnvironmentState) { v.UpdatedAt = time.Time{} }, field: "updatedAt"},
		{name: "last_pair", base: ready, mutate: func(v *EnvironmentState) { v.LastSuccessful.Commit = "" }, field: "lastSuccessful"},
		{name: "ready_without_revision", base: ready, mutate: func(v *EnvironmentState) { v.LastSuccessful = Revision{} }, field: "lastSuccessful"},
		{name: "ready_with_broken", base: ready, mutate: func(v *EnvironmentState) { details := validRepositoryChanged(store); v.Broken = &details }, field: "broken"},
		{name: "broken_without_details", base: broken, mutate: func(v *EnvironmentState) { v.Broken = nil }, field: "broken"},
		{name: "target", base: broken, mutate: func(v *EnvironmentState) { v.Broken.TargetVersion = "" }, field: "targetVersion"},
		{name: "branch", base: broken, mutate: func(v *EnvironmentState) { v.Broken.Branch = "release/v5.3.0" }, field: "branch"},
		{name: "commit", base: broken, mutate: func(v *EnvironmentState) { v.Broken.Commit = "bad" }, field: "commit"},
		{name: "reason", base: broken, mutate: func(v *EnvironmentState) { v.Broken.Reason = BrokenReason("future") }, field: "reason"},
		{name: "stage", base: broken, mutate: func(v *EnvironmentState) { v.Broken.Stage = protocol.StageBackendRun }, field: "stage"},
		{name: "exit", base: broken, mutate: func(v *EnvironmentState) { v.Broken.ExitCode = 0 }, field: "exitCode"},
		{name: "log_outside", base: broken, mutate: func(v *EnvironmentState) { v.Broken.LogPath = filepath.Join(store.layout.AppRoot(), "outside.log") }, field: "logPath"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			value := test.base
			if value.Broken != nil {
				copyOfBroken := *value.Broken
				value.Broken = &copyOfBroken
			}
			test.mutate(&value)
			var validationErr *ValidationError
			err := store.ValidateEnvironment(value)
			if !errors.As(err, &validationErr) {
				t.Fatalf("ValidateEnvironment() error = %v, want *ValidationError", err)
			}
			if validationErr.Field != test.field {
				t.Fatalf("ValidationError.Field = %q, want %q", validationErr.Field, test.field)
			}
		})
	}
}

func TestValidateEnvironment_RepositoryChangedRequiresSwapSemantics(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, completeFakeStateFiles(), fixedStateTime)
	tests := []struct {
		name   string
		mutate func(*BrokenEnvironment)
	}{
		{name: "non_swap", mutate: func(v *BrokenEnvironment) { v.Stage = protocol.StageWorkspaceCleanup }},
		{name: "nonzero_exit", mutate: func(v *BrokenEnvironment) { v.ExitCode = 40 }},
		{name: "operation_failed_swap", mutate: func(v *BrokenEnvironment) {
			v.Reason = ReasonOperationFailed
			v.PythonVersion = "3.12.10"
			v.UVVersion = "0.8.0"
			v.ExitCode = 40
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			details := validRepositoryChanged(store)
			test.mutate(&details)
			value := EnvironmentState{
				SchemaVersion:  SchemaVersion,
				Status:         protocol.StateEnvironmentBroken,
				UpdatedAt:      fixedStateTime(),
				LastSuccessful: validLastSuccessful(),
				Broken:         &details,
			}
			if err := store.ValidateEnvironment(value); err == nil {
				t.Fatal("ValidateEnvironment() error = nil, want reason/stage rejection")
			}
		})
	}
}

func TestValidateEnvironment_BrokenAllowsNoLastSuccessful(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, completeFakeStateFiles(), fixedStateTime)
	value, err := store.NewBrokenEnvironment(
		Revision{},
		validRepositoryChanged(store),
	)
	if err != nil {
		t.Fatalf("NewBrokenEnvironment(empty revision) error = %v", err)
	}
	if value.LastSuccessful != (Revision{}) {
		t.Fatalf("LastSuccessful = %#v, want empty revision", value.LastSuccessful)
	}
}

func TestValidateEnvironment_OperationFailedToolRequirements(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, completeFakeStateFiles(), fixedStateTime)
	stageRequirements := []struct {
		stage         protocol.Stage
		toolsRequired bool
	}{
		{stage: protocol.StageWorkspaceCleanup},
		{stage: protocol.StagePythonCheck},
		{stage: protocol.StagePythonInstall, toolsRequired: true},
		{stage: protocol.StageDependenciesCheck, toolsRequired: true},
		{stage: protocol.StageDependenciesSync, toolsRequired: true},
		{stage: protocol.StageDependenciesRebuild, toolsRequired: true},
	}
	toolCases := []struct {
		name   string
		python string
		uv     string
	}{
		{name: "complete", python: "3.12.10", uv: "0.8.0"},
		{name: "missing_python", uv: "0.8.0"},
		{name: "missing_uv", python: "3.12.10"},
	}
	for _, requirement := range stageRequirements {
		requirement := requirement
		t.Run(string(requirement.stage), func(t *testing.T) {
			for _, toolCase := range toolCases {
				toolCase := toolCase
				t.Run(toolCase.name, func(t *testing.T) {
					details := validOperationFailed(store)
					details.Stage = requirement.stage
					details.PythonVersion = toolCase.python
					details.UVVersion = toolCase.uv
					value := EnvironmentState{
						SchemaVersion:  SchemaVersion,
						Status:         protocol.StateEnvironmentBroken,
						UpdatedAt:      fixedStateTime(),
						LastSuccessful: Revision{},
						Broken:         &details,
					}
					err := store.ValidateEnvironment(value)
					wantErr := requirement.toolsRequired && toolCase.name != "complete"
					if (err != nil) != wantErr {
						t.Fatalf(
							"ValidateEnvironment(stage=%q, tools=%s) error = %v, wantErr %t",
							requirement.stage,
							toolCase.name,
							err,
							wantErr,
						)
					}
				})
			}
		})
	}
}

func TestValidateEnvironment_RejectsUnresolvedRepositoryFailure(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, completeFakeStateFiles(), fixedStateTime)
	for _, stage := range []protocol.Stage{
		protocol.StageUVCheck,
		protocol.StageUVDownload,
		protocol.StageUVVerify,
		protocol.StageWorkspaceSwap,
		protocol.StageBootstrap,
		protocol.StageRepair,
		protocol.StageBackendRun,
	} {
		details := validOperationFailed(store)
		details.Stage = stage
		value := EnvironmentState{
			SchemaVersion:  SchemaVersion,
			Status:         protocol.StateEnvironmentBroken,
			UpdatedAt:      fixedStateTime(),
			LastSuccessful: validLastSuccessful(),
			Broken:         &details,
		}
		if err := store.ValidateEnvironment(value); err == nil {
			t.Fatalf("ValidateEnvironment(stage=%q) error = nil, want rejection", stage)
		}
	}
}

func TestValidateEnvironment_UsesSharedSafeVersionRules(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, completeFakeStateFiles(), fixedStateTime)
	tests := []struct {
		name   string
		mutate func(*EnvironmentState)
	}{
		{name: "last_version", mutate: func(v *EnvironmentState) { v.LastSuccessful.Version = "v5..3" }},
		{name: "target_version", mutate: func(v *EnvironmentState) { v.Broken.TargetVersion = "v5@{1}" }},
		{name: "python_path", mutate: func(v *EnvironmentState) { v.Broken.PythonVersion = `3.12\bin` }},
		{name: "uv_control", mutate: func(v *EnvironmentState) { v.Broken.UVVersion = "0.8\n" }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			details := validOperationFailed(store)
			value := EnvironmentState{
				SchemaVersion:  SchemaVersion,
				Status:         protocol.StateEnvironmentBroken,
				UpdatedAt:      fixedStateTime(),
				LastSuccessful: validLastSuccessful(),
				Broken:         &details,
			}
			test.mutate(&value)
			if err := store.ValidateEnvironment(value); err == nil {
				t.Fatal("ValidateEnvironment() error = nil, want safe-version rejection")
			}
		})
	}
}

func TestStore_UVPreparationFailureDoesNotCreateBrokenState(t *testing.T) {
	t.Parallel()

	writeCalls := 0
	files := completeFakeStateFiles()
	files.write = func(
		context.Context,
		filesystem.StateFileKind,
		[]byte,
	) (writeResult, error) {
		writeCalls++
		return writeResult{mutationApplied: true}, nil
	}
	store := newTestStore(t, files, fixedStateTime)
	details := validOperationFailed(store)
	details.Stage = protocol.StageUVVerify
	if _, err := store.NewBrokenEnvironment(validLastSuccessful(), details); err == nil {
		t.Fatal("NewBrokenEnvironment(uv.verify) error = nil, want rejection")
	}
	if writeCalls != 0 {
		t.Fatalf("WriteAtomic calls = %d, want 0", writeCalls)
	}
}
