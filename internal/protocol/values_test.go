package protocol_test

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestStableValueSets(t *testing.T) {
	t.Parallel()

	wantStages := []protocol.Stage{
		protocol.StageRuntimeHandshake, protocol.StageDoctor, protocol.StageBootstrap, protocol.StageRepair,
		protocol.StageCleanup, protocol.StageUVCheck, protocol.StageUVDownload, protocol.StageUVVerify,
		protocol.StageWorkspaceCheck, protocol.StageWorkspaceClone, protocol.StageWorkspaceVerify,
		protocol.StageWorkspaceSwap, protocol.StageWorkspaceCleanup, protocol.StagePythonCheck,
		protocol.StagePythonInstall, protocol.StageDependenciesCheck, protocol.StageDependenciesSync,
		protocol.StageDependenciesRebuild, protocol.StageBackendSpawn, protocol.StageBackendHealth,
		protocol.StageBackendRun, protocol.StageBackendRestart, protocol.StageBackendShutdown,
		protocol.StageBackendCleanup,
	}
	if got := protocol.AllStages(); !reflect.DeepEqual(got, wantStages) {
		t.Fatalf("AllStages() = %#v, want %#v", got, wantStages)
	}

	wantProgress := []protocol.ProgressStatus{
		protocol.ProgressPending, protocol.ProgressRunning, protocol.ProgressSucceeded,
		protocol.ProgressSkipped, protocol.ProgressFailed, protocol.ProgressCancelled,
	}
	if got := protocol.AllProgressStatuses(); !reflect.DeepEqual(got, wantProgress) {
		t.Fatalf("AllProgressStatuses() = %#v, want %#v", got, wantProgress)
	}

	wantStates := []protocol.StateStatus{
		protocol.StateUninitialized, protocol.StatePreparingUV, protocol.StateSyncingRepository,
		protocol.StatePreparingPython, protocol.StateSyncingEnvironment, protocol.StateReadyToStart,
		protocol.StateStartingBackend, protocol.StateRunning, protocol.StateRestarting,
		protocol.StateStoppingBackend, protocol.StateEnvironmentBroken, protocol.StateBackendFailed,
		protocol.StateStopped,
	}
	if got := protocol.AllStateStatuses(); !reflect.DeepEqual(got, wantStates) {
		t.Fatalf("AllStateStatuses() = %#v, want %#v", got, wantStates)
	}
}

func TestProtocolValuesMatchArchitectureDocument(t *testing.T) {
	t.Parallel()

	stages, progressStatuses, stateStatuses := documentedProtocolValues(t)
	if got := protocol.AllStages(); !reflect.DeepEqual(got, stages) {
		t.Fatalf("AllStages() = %#v, documented = %#v", got, stages)
	}
	if got := protocol.AllProgressStatuses(); !reflect.DeepEqual(got, progressStatuses) {
		t.Fatalf("AllProgressStatuses() = %#v, documented = %#v", got, progressStatuses)
	}
	if got := protocol.AllStateStatuses(); !reflect.DeepEqual(got, stateStatuses) {
		t.Fatalf("AllStateStatuses() = %#v, documented = %#v", got, stateStatuses)
	}
	if got, want := protocol.AllCapabilities(), documentedCapabilities(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("AllCapabilities() = %#v, documented = %#v", got, want)
	}
}

func TestProtocolValueQueriesReturnDefensiveCopies(t *testing.T) {
	t.Parallel()

	stages := protocol.AllStages()
	stages[0] = protocol.Stage("changed")
	if got := protocol.AllStages()[0]; got != protocol.StageRuntimeHandshake {
		t.Fatalf("AllStages() exposed shared storage: %q", got)
	}

	progressStatuses := protocol.AllProgressStatuses()
	progressStatuses[0] = protocol.ProgressStatus("changed")
	if got := protocol.AllProgressStatuses()[0]; got != protocol.ProgressPending {
		t.Fatalf("AllProgressStatuses() exposed shared storage: %q", got)
	}

	stateStatuses := protocol.AllStateStatuses()
	stateStatuses[0] = protocol.StateStatus("changed")
	if got := protocol.AllStateStatuses()[0]; got != protocol.StateUninitialized {
		t.Fatalf("AllStateStatuses() exposed shared storage: %q", got)
	}

	capabilities := protocol.AllCapabilities()
	capabilities[0] = protocol.Capability("changed")
	if got := protocol.AllCapabilities()[0]; got != protocol.CapabilityStdinCancel {
		t.Fatalf("AllCapabilities() exposed shared storage: %q", got)
	}
}

func TestProtocolValueKnownQueries(t *testing.T) {
	t.Parallel()

	for _, stage := range protocol.AllStages() {
		if !protocol.IsKnownStage(stage) {
			t.Errorf("IsKnownStage(%q) = false, want true", stage)
		}
	}
	if protocol.IsKnownStage(protocol.Stage("future.stage")) {
		t.Error("IsKnownStage(future.stage) = true, want false")
	}

	for _, status := range protocol.AllProgressStatuses() {
		if !protocol.IsKnownProgressStatus(status) {
			t.Errorf("IsKnownProgressStatus(%q) = false, want true", status)
		}
	}
	if protocol.IsKnownProgressStatus(protocol.ProgressStatus("future")) {
		t.Error("IsKnownProgressStatus(future) = true, want false")
	}

	for _, status := range protocol.AllStateStatuses() {
		if !protocol.IsKnownStateStatus(status) {
			t.Errorf("IsKnownStateStatus(%q) = false, want true", status)
		}
	}
	if protocol.IsKnownStateStatus(protocol.StateStatus("future")) {
		t.Error("IsKnownStateStatus(future) = true, want false")
	}

	for _, capability := range protocol.AllCapabilities() {
		if !protocol.IsKnownCapability(capability) {
			t.Errorf("IsKnownCapability(%q) = false, want true", capability)
		}
	}
	if protocol.IsKnownCapability(protocol.Capability("future.capability")) {
		t.Error("IsKnownCapability(future.capability) = true, want false")
	}
}

func TestTypedEventJSONCompatibility(t *testing.T) {
	t.Parallel()

	progress := protocol.ProgressEvent{
		Stage: protocol.StageDependenciesSync, Status: protocol.ProgressRunning,
	}
	progressJSON, err := json.Marshal(progress)
	if err != nil {
		t.Fatalf("marshal progress event: %v", err)
	}
	if !strings.Contains(string(progressJSON), `"stage":"dependencies.sync"`) ||
		!strings.Contains(string(progressJSON), `"status":"running"`) {
		t.Fatalf("progress JSON = %s", progressJSON)
	}
	var decodedProgress protocol.ProgressEvent
	if err := json.Unmarshal(progressJSON, &decodedProgress); err != nil {
		t.Fatalf("unmarshal progress event: %v", err)
	}
	if !reflect.DeepEqual(decodedProgress, progress) {
		t.Fatalf("decoded progress = %#v, want %#v", decodedProgress, progress)
	}

	state := protocol.StateEvent{
		Stage: protocol.StageDependenciesSync, Status: protocol.StateSyncingEnvironment,
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state event: %v", err)
	}
	if !strings.Contains(string(stateJSON), `"stage":"dependencies.sync"`) ||
		!strings.Contains(string(stateJSON), `"status":"syncing_environment"`) {
		t.Fatalf("state JSON = %s", stateJSON)
	}
	var decodedState protocol.StateEvent
	if err := json.Unmarshal(stateJSON, &decodedState); err != nil {
		t.Fatalf("unmarshal state event: %v", err)
	}
	if !reflect.DeepEqual(decodedState, state) {
		t.Fatalf("decoded state = %#v, want %#v", decodedState, state)
	}
}

func TestUnknownStageDecodesWithoutProtocolRejection(t *testing.T) {
	t.Parallel()

	var event protocol.ProgressEvent
	if err := json.Unmarshal([]byte(`{"stage":"future.stage","status":"running"}`), &event); err != nil {
		t.Fatalf("unmarshal future stage: %v", err)
	}
	if event.Stage != protocol.Stage("future.stage") {
		t.Fatalf("stage = %q, want future.stage", event.Stage)
	}
	if protocol.IsKnownStage(event.Stage) {
		t.Error("IsKnownStage(future.stage) = true, want false")
	}
}

func documentedProtocolValues(t *testing.T) ([]protocol.Stage, []protocol.ProgressStatus, []protocol.StateStatus) {
	t.Helper()

	content := readDoc(t, "架构设计.md")

	stageSection := docSection(t, content, "协议版本 1 的 `stage` 标识固定如下", "新增 stage 可以追加")
	stageMatches := regexp.MustCompile("`([a-z]+(?:\\.[a-z]+)?)`").FindAllStringSubmatch(stageSection, -1)
	stages := make([]protocol.Stage, 0, len(stageMatches))
	for _, match := range stageMatches {
		stages = append(stages, protocol.Stage(match[1]))
	}

	progressSection := docSection(t, content, "`progress.status` 在协议版本 1 中只能是", "协议版本 1 的 `stage` 标识固定如下")
	progressSentence, _, found := strings.Cut(progressSection, "。")
	if !found {
		t.Fatal("progress.status sentence is not terminated")
	}
	progressMatches := regexp.MustCompile("`([a-z]+)`").FindAllStringSubmatch(progressSentence, -1)
	progressStatuses := make([]protocol.ProgressStatus, 0, len(progressMatches))
	for _, match := range progressMatches {
		progressStatuses = append(progressStatuses, protocol.ProgressStatus(match[1]))
	}

	stateSection := docSection(t, content, "协议版本 1 的 `state.status` 取值固定如下", "主准备路径固定为")
	stateMatches := regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|").FindAllStringSubmatch(stateSection, -1)
	stateStatuses := make([]protocol.StateStatus, 0, len(stateMatches))
	for _, match := range stateMatches {
		stateStatuses = append(stateStatuses, protocol.StateStatus(match[1]))
	}

	if len(stages) != 24 || len(progressStatuses) != 6 || len(stateStatuses) != 13 {
		t.Fatalf("documented value counts = stages:%d progress:%d states:%d, want 24/6/13", len(stages), len(progressStatuses), len(stateStatuses))
	}
	return stages, progressStatuses, stateStatuses
}

func documentedCapabilities(t *testing.T) []protocol.Capability {
	t.Helper()

	section := docSection(
		t,
		readDoc(t, "架构设计.md"),
		"协议版本 1 的能力标识固定如下",
		"新增能力标识可以追加",
	)
	matches := regexp.MustCompile("(?m)^\\| `([a-z0-9.]+)` \\|").FindAllStringSubmatch(section, -1)
	capabilities := make([]protocol.Capability, 0, len(matches))
	for _, match := range matches {
		capabilities = append(capabilities, protocol.Capability(match[1]))
	}
	if len(capabilities) != 3 {
		t.Fatalf("documented capability count = %d, want 3", len(capabilities))
	}
	return capabilities
}
