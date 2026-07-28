package protocol

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

type controlContractHandler struct {
	disposition ControlDisposition
	prepareErr  error
	actionErr   error
	stage       Stage
	prepared    []ControlCommand
	actions     int
}

func (h *controlContractHandler) PrepareControl(command ControlCommand) (ControlDisposition, ControlAction, error) {
	h.prepared = append(h.prepared, command)
	if h.prepareErr != nil {
		return ControlDispositionUnknown, nil, h.prepareErr
	}
	disposition := h.disposition
	if disposition == ControlDispositionUnknown {
		disposition = ControlAccepted
	}
	if disposition == ControlNotApplicable {
		return disposition, nil, nil
	}
	return disposition, func() error {
		h.actions++
		return h.actionErr
	}, nil
}

func (h *controlContractHandler) CurrentControlStage() Stage {
	if h.stage == "" {
		return StageBackendRun
	}
	return h.stage
}

type controlContractWarningEmitter struct {
	warnings []WarningEvent
	err      error
}

func (e *controlContractWarningEmitter) EmitWarning(warning WarningEvent) error {
	e.warnings = append(e.warnings, warning)
	return e.err
}

var (
	_ ControlHandler        = (*controlContractHandler)(nil)
	_ ControlWarningEmitter = (*controlContractWarningEmitter)(nil)
)

type controlResultHandler struct {
	disposition ControlDisposition
	action      ControlAction
	prepareErr  error
	stage       Stage
	prepared    []ControlCommand
}

func (h *controlResultHandler) PrepareControl(command ControlCommand) (ControlDisposition, ControlAction, error) {
	h.prepared = append(h.prepared, command)
	return h.disposition, h.action, h.prepareErr
}

func (h *controlResultHandler) CurrentControlStage() Stage {
	return h.stage
}

type latchingControlHandler struct {
	cancelPrepared   int
	cancelActions    int
	cancelEffects    int
	cancelLatched    bool
	shutdownPrepared int
	shutdownActions  int
	statusPrepared   int
	statusSnapshots  []string
}

func (h *latchingControlHandler) PrepareControl(command ControlCommand) (ControlDisposition, ControlAction, error) {
	switch command.Command {
	case ControlCancel:
		h.cancelPrepared++
		return ControlAccepted, func() error {
			h.cancelActions++
			if !h.cancelLatched {
				h.cancelLatched = true
				h.cancelEffects++
			}
			return nil
		}, nil
	case ControlShutdown:
		h.shutdownPrepared++
		return ControlAccepted, func() error {
			h.shutdownActions++
			return nil
		}, nil
	case ControlStatus:
		h.statusPrepared++
		return ControlAccepted, func() error {
			h.statusSnapshots = append(h.statusSnapshots, command.CommandID)
			return nil
		}, nil
	default:
		panic(fmt.Sprintf("unexpected control command %q", command.Command))
	}
}

func (*latchingControlHandler) CurrentControlStage() Stage {
	return StageBackendRun
}

func controlTestID(index int) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	id := []byte("01J00000000000000000000000")
	for offset := len(id) - 1; index > 0; offset-- {
		id[offset] = alphabet[index%len(alphabet)]
		index /= len(alphabet)
	}
	return string(id)
}

func controlTestLine(command ControlKind, commandID string) string {
	return fmt.Sprintf(
		`{"protocol":1,"command":%q,"commandId":%q}`,
		command,
		commandID,
	)
}

func TestControlContract_Kinds(t *testing.T) {
	tests := []struct {
		name string
		kind ControlKind
		want string
	}{
		{name: "cancel", kind: ControlCancel, want: "cancel"},
		{name: "shutdown", kind: ControlShutdown, want: "shutdown"},
		{name: "status", kind: ControlStatus, want: "status"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(tt.kind); got != tt.want {
				t.Fatalf("ControlKind = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestControlContract_CommandJSONFields(t *testing.T) {
	command := ControlCommand{
		Protocol:  1,
		Command:   ControlCancel,
		CommandID: "01J00000000000000000000000",
	}

	got, err := json.Marshal(command)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	const want = `{"protocol":1,"command":"cancel","commandId":"01J00000000000000000000000"}`
	if string(got) != want {
		t.Fatalf("json.Marshal() = %s, want %s", got, want)
	}
}

func TestControlContract_DispositionsAndAction(t *testing.T) {
	if ControlDispositionUnknown != 0 {
		t.Fatalf("ControlDispositionUnknown = %d, want zero value", ControlDispositionUnknown)
	}
	if ControlAccepted == ControlDispositionUnknown {
		t.Fatal("ControlAccepted must be a non-zero disposition")
	}
	if ControlNotApplicable == ControlDispositionUnknown {
		t.Fatal("ControlNotApplicable must be a non-zero disposition")
	}
	if ControlAccepted == ControlNotApplicable {
		t.Fatal("accepted and not-applicable dispositions must differ")
	}

	called := false
	var action ControlAction = func() error {
		called = true
		return nil
	}
	if err := action(); err != nil {
		t.Fatalf("ControlAction() error = %v", err)
	}
	if !called {
		t.Fatal("ControlAction was not invoked")
	}
}

func TestWithControlCommandID(t *testing.T) {
	t.Run("nil details", func(t *testing.T) {
		got := WithControlCommandID(nil, "01J00000000000000000000000")

		if len(got) != 1 {
			t.Fatalf("len(details) = %d, want 1", len(got))
		}
		if got["controlCommandId"] != "01J00000000000000000000000" {
			t.Fatalf("controlCommandId = %#v", got["controlCommandId"])
		}
	})

	t.Run("copies existing details", func(t *testing.T) {
		original := map[string]any{"attempt": 2}

		got := WithControlCommandID(original, "01J00000000000000000000001")

		if got["attempt"] != 2 {
			t.Fatalf("attempt = %#v, want 2", got["attempt"])
		}
		if got["controlCommandId"] != "01J00000000000000000000001" {
			t.Fatalf("controlCommandId = %#v", got["controlCommandId"])
		}
		got["attempt"] = 3
		if original["attempt"] != 2 {
			t.Fatalf("original was mutated: %#v", original)
		}
	})

	t.Run("overrides copied command id", func(t *testing.T) {
		original := map[string]any{
			"controlCommandId": "old",
			"state":            "running",
		}

		got := WithControlCommandID(original, "01J00000000000000000000002")

		if got["controlCommandId"] != "01J00000000000000000000002" {
			t.Fatalf("controlCommandId = %#v", got["controlCommandId"])
		}
		if original["controlCommandId"] != "old" {
			t.Fatalf("original command id was mutated: %#v", original)
		}
	})
}

type typedNilControlInput struct{}

func (*typedNilControlInput) Read([]byte) (int, error) {
	return 0, io.EOF
}

func TestControlReader_ConstructorValidation(t *testing.T) {
	validInput := strings.NewReader("")
	validWarnings := &controlContractWarningEmitter{}
	validHandler := &controlContractHandler{}

	var nilInput *typedNilControlInput
	var nilWarnings *controlContractWarningEmitter
	var nilHandler *controlContractHandler

	tests := []struct {
		name     string
		input    io.Reader
		warnings ControlWarningEmitter
		handler  ControlHandler
		allowed  []ControlKind
		wantErr  string
	}{
		{
			name:     "nil input",
			input:    nil,
			warnings: validWarnings,
			handler:  validHandler,
			allowed:  []ControlKind{ControlCancel},
			wantErr:  "input must not be nil",
		},
		{
			name:     "typed nil input",
			input:    nilInput,
			warnings: validWarnings,
			handler:  validHandler,
			allowed:  []ControlKind{ControlCancel},
			wantErr:  "input must not be nil",
		},
		{
			name:     "nil warning emitter",
			input:    validInput,
			warnings: nil,
			handler:  validHandler,
			allowed:  []ControlKind{ControlCancel},
			wantErr:  "warning emitter must not be nil",
		},
		{
			name:     "typed nil warning emitter",
			input:    validInput,
			warnings: nilWarnings,
			handler:  validHandler,
			allowed:  []ControlKind{ControlCancel},
			wantErr:  "warning emitter must not be nil",
		},
		{
			name:     "nil handler",
			input:    validInput,
			warnings: validWarnings,
			handler:  nil,
			allowed:  []ControlKind{ControlCancel},
			wantErr:  "control handler must not be nil",
		},
		{
			name:     "typed nil handler",
			input:    validInput,
			warnings: validWarnings,
			handler:  nilHandler,
			allowed:  []ControlKind{ControlCancel},
			wantErr:  "control handler must not be nil",
		},
		{
			name:     "empty allowed set",
			input:    validInput,
			warnings: validWarnings,
			handler:  validHandler,
			wantErr:  "at least one allowed control command is required",
		},
		{
			name:     "unknown allowed command",
			input:    validInput,
			warnings: validWarnings,
			handler:  validHandler,
			allowed:  []ControlKind{"pause"},
			wantErr:  `unknown allowed control command "pause"`,
		},
		{
			name:     "duplicate allowed command",
			input:    validInput,
			warnings: validWarnings,
			handler:  validHandler,
			allowed:  []ControlKind{ControlCancel, ControlCancel},
			wantErr:  `duplicate allowed control command "cancel"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, err := NewControlReader(tt.input, tt.warnings, tt.handler, tt.allowed...)
			if reader != nil {
				t.Fatalf("NewControlReader() reader = %#v, want nil", reader)
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("NewControlReader() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestControlReader_UsesFixedBufferWhenInputIsAlreadyBuffered(t *testing.T) {
	input := bufio.NewReaderSize(strings.NewReader(""), 64*1024)

	reader, err := NewControlReader(
		input,
		&controlContractWarningEmitter{},
		&controlContractHandler{},
		ControlCancel,
	)
	if err != nil {
		t.Fatalf("NewControlReader() error = %v", err)
	}
	if got := reader.input.Size(); got != controlReaderBufferSize {
		t.Fatalf("control reader buffer size = %d, want %d", got, controlReaderBufferSize)
	}
}

func TestControlReader_FramingAndAcceptedCancel(t *testing.T) {
	const commandLine = `{"protocol":1,"command":"cancel","commandId":"01J00000000000000000000000"}`
	tests := []struct {
		name        string
		input       string
		wantActions int
	}{
		{name: "LF", input: commandLine + "\n", wantActions: 1},
		{name: "CRLF", input: commandLine + "\r\n", wantActions: 1},
		{name: "final line without newline", input: commandLine, wantActions: 1},
		{name: "empty EOF", input: "", wantActions: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &controlContractHandler{}
			warnings := &controlContractWarningEmitter{}
			reader, err := NewControlReader(strings.NewReader(tt.input), warnings, handler, ControlCancel)
			if err != nil {
				t.Fatalf("NewControlReader() error = %v", err)
			}

			if err := reader.Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if handler.actions != tt.wantActions {
				t.Fatalf("action calls = %d, want %d", handler.actions, tt.wantActions)
			}
			if len(handler.prepared) != tt.wantActions {
				t.Fatalf("prepared commands = %d, want %d", len(handler.prepared), tt.wantActions)
			}
			if len(warnings.warnings) != 0 {
				t.Fatalf("warnings = %#v, want none", warnings.warnings)
			}
			if tt.wantActions == 1 {
				want := ControlCommand{
					Protocol:  1,
					Command:   ControlCancel,
					CommandID: "01J00000000000000000000000",
				}
				if !reflect.DeepEqual(handler.prepared[0], want) {
					t.Fatalf("prepared command = %#v, want %#v", handler.prepared[0], want)
				}
			}
		})
	}
}

func TestControlReader_DispatchesAllCommandsAndReturnsEveryStatusSnapshot(t *testing.T) {
	handler := &latchingControlHandler{}
	warnings := &controlContractWarningEmitter{}
	input := strings.Join([]string{
		controlTestLine(ControlCancel, controlTestID(1)),
		controlTestLine(ControlShutdown, controlTestID(2)),
		controlTestLine(ControlStatus, controlTestID(3)),
		controlTestLine(ControlStatus, controlTestID(4)),
	}, "\n") + "\n"
	reader, err := NewControlReader(
		strings.NewReader(input),
		warnings,
		handler,
		ControlCancel,
		ControlShutdown,
		ControlStatus,
	)
	if err != nil {
		t.Fatalf("NewControlReader() error = %v", err)
	}

	if err := reader.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if handler.cancelPrepared != 1 || handler.cancelActions != 1 || handler.cancelEffects != 1 {
		t.Fatalf(
			"cancel prepared/actions/effects = %d/%d/%d, want 1/1/1",
			handler.cancelPrepared,
			handler.cancelActions,
			handler.cancelEffects,
		)
	}
	if handler.shutdownPrepared != 1 || handler.shutdownActions != 1 {
		t.Fatalf(
			"shutdown prepared/actions = %d/%d, want 1/1",
			handler.shutdownPrepared,
			handler.shutdownActions,
		)
	}
	if handler.statusPrepared != 2 {
		t.Fatalf("status prepared = %d, want 2", handler.statusPrepared)
	}
	if want := []string{controlTestID(3), controlTestID(4)}; !reflect.DeepEqual(handler.statusSnapshots, want) {
		t.Fatalf("status snapshots = %#v, want %#v", handler.statusSnapshots, want)
	}
	if len(warnings.warnings) != 0 {
		t.Fatalf("warnings = %#v, want none", warnings.warnings)
	}
}

func TestControlReader_ApplicabilityWarnsOnceAndRecovers(t *testing.T) {
	t.Run("static disallow and bad input recover to allowed command", func(t *testing.T) {
		const disallowedID = "01J00000000000000000000001"
		handler := &latchingControlHandler{}
		warnings := &controlContractWarningEmitter{}
		input := "{bad json}\n" +
			controlTestLine(ControlStatus, disallowedID) + "\n" +
			controlTestLine(ControlStatus, disallowedID) + "\n" +
			controlTestLine(ControlCancel, controlTestID(2)) + "\n"
		reader, err := NewControlReader(strings.NewReader(input), warnings, handler, ControlCancel)
		if err != nil {
			t.Fatalf("NewControlReader() error = %v", err)
		}

		if err := reader.Run(context.Background()); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if handler.statusPrepared != 0 {
			t.Fatalf("status prepared = %d, want 0", handler.statusPrepared)
		}
		if handler.cancelActions != 1 {
			t.Fatalf("cancel actions = %d, want 1", handler.cancelActions)
		}
		if len(warnings.warnings) != 2 {
			t.Fatalf("warning count = %d, want 2: %#v", len(warnings.warnings), warnings.warnings)
		}
		assertInvalidControlWarning(t, warnings.warnings[1], map[string]any{
			"reason":           "command_not_applicable",
			"command":          ControlStatus,
			"controlCommandId": disallowedID,
		})
	})

	t.Run("dynamic not applicable is recorded before warning", func(t *testing.T) {
		const commandID = "01J00000000000000000000003"
		handler := &controlResultHandler{
			disposition: ControlNotApplicable,
			stage:       StageBackendRun,
		}
		warnings := &controlContractWarningEmitter{}
		line := controlTestLine(ControlShutdown, commandID)
		reader, err := NewControlReader(
			strings.NewReader(line+"\n"+line+"\n"),
			warnings,
			handler,
			ControlShutdown,
		)
		if err != nil {
			t.Fatalf("NewControlReader() error = %v", err)
		}

		if err := reader.Run(context.Background()); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if len(handler.prepared) != 1 {
			t.Fatalf("prepared commands = %d, want 1", len(handler.prepared))
		}
		if len(warnings.warnings) != 1 {
			t.Fatalf("warning count = %d, want 1", len(warnings.warnings))
		}
		assertInvalidControlWarning(t, warnings.warnings[0], map[string]any{
			"reason":           "command_not_applicable",
			"command":          ControlShutdown,
			"controlCommandId": commandID,
		})
	})
}

func TestControlReader_IdempotencyUsesBoundedFIFOLedger(t *testing.T) {
	t.Run("same ID is silent and conflicting reuse reports original command", func(t *testing.T) {
		const commandID = "01J00000000000000000000001"
		handler := &latchingControlHandler{}
		warnings := &controlContractWarningEmitter{}
		input := controlTestLine(ControlCancel, commandID) + "\n" +
			controlTestLine(ControlCancel, commandID) + "\n" +
			controlTestLine(ControlStatus, commandID) + "\n"
		reader, err := NewControlReader(
			strings.NewReader(input),
			warnings,
			handler,
			ControlCancel,
			ControlStatus,
		)
		if err != nil {
			t.Fatalf("NewControlReader() error = %v", err)
		}

		if err := reader.Run(context.Background()); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if handler.cancelPrepared != 1 || handler.cancelActions != 1 {
			t.Fatalf(
				"cancel prepared/actions = %d/%d, want 1/1",
				handler.cancelPrepared,
				handler.cancelActions,
			)
		}
		if handler.statusPrepared != 0 {
			t.Fatalf("status prepared = %d, want 0", handler.statusPrepared)
		}
		if len(warnings.warnings) != 1 {
			t.Fatalf("warning count = %d, want 1", len(warnings.warnings))
		}
		assertInvalidControlWarning(t, warnings.warnings[0], map[string]any{
			"reason":           "command_id_reused",
			"command":          ControlStatus,
			"controlCommandId": commandID,
			"originalCommand":  ControlCancel,
		})
	})

	t.Run("cancel latch survives different IDs and ledger eviction", func(t *testing.T) {
		handler := &latchingControlHandler{}
		warnings := &controlContractWarningEmitter{}
		var input strings.Builder
		firstCancelID := controlTestID(1)
		input.WriteString(controlTestLine(ControlCancel, firstCancelID))
		input.WriteByte('\n')
		input.WriteString(controlTestLine(ControlCancel, controlTestID(2)))
		input.WriteByte('\n')
		for i := 3; i <= 1025; i++ {
			input.WriteString(controlTestLine(ControlStatus, controlTestID(i)))
			input.WriteByte('\n')
		}
		input.WriteString(controlTestLine(ControlCancel, firstCancelID))
		input.WriteByte('\n')

		reader, err := NewControlReader(
			strings.NewReader(input.String()),
			warnings,
			handler,
			ControlCancel,
			ControlStatus,
		)
		if err != nil {
			t.Fatalf("NewControlReader() error = %v", err)
		}
		if err := reader.Run(context.Background()); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if handler.cancelPrepared != 3 || handler.cancelActions != 3 {
			t.Fatalf(
				"cancel prepared/actions = %d/%d, want 3/3",
				handler.cancelPrepared,
				handler.cancelActions,
			)
		}
		if handler.cancelEffects != 1 {
			t.Fatalf("cancel side effects = %d, want 1", handler.cancelEffects)
		}
		if len(warnings.warnings) != 0 {
			t.Fatalf("warnings = %#v, want none", warnings.warnings)
		}
	})

	t.Run("1024 status commands do not block a new shutdown", func(t *testing.T) {
		handler := &latchingControlHandler{}
		warnings := &controlContractWarningEmitter{}
		var input strings.Builder
		for i := 1; i <= 1024; i++ {
			input.WriteString(controlTestLine(ControlStatus, controlTestID(i)))
			input.WriteByte('\n')
		}
		input.WriteString(controlTestLine(ControlShutdown, controlTestID(1025)))
		input.WriteByte('\n')

		reader, err := NewControlReader(
			strings.NewReader(input.String()),
			warnings,
			handler,
			ControlStatus,
			ControlShutdown,
		)
		if err != nil {
			t.Fatalf("NewControlReader() error = %v", err)
		}
		if err := reader.Run(context.Background()); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if handler.statusPrepared != 1024 || len(handler.statusSnapshots) != 1024 {
			t.Fatalf(
				"status prepared/snapshots = %d/%d, want 1024/1024",
				handler.statusPrepared,
				len(handler.statusSnapshots),
			)
		}
		if handler.shutdownPrepared != 1 || handler.shutdownActions != 1 {
			t.Fatalf(
				"shutdown prepared/actions = %d/%d, want 1/1",
				handler.shutdownPrepared,
				handler.shutdownActions,
			)
		}
	})
}

func TestControlReader_ErrorsPropagateExactly(t *testing.T) {
	prepareErr := errors.New("prepare failed")
	actionErr := errors.New("action failed")
	warningErr := errors.New("warning failed")
	tests := []struct {
		name       string
		handler    ControlHandler
		warnings   *controlContractWarningEmitter
		input      string
		allowed    []ControlKind
		wantErr    string
		wantTarget error
	}{
		{
			name: "prepare error",
			handler: &controlResultHandler{
				disposition: ControlDispositionUnknown,
				prepareErr:  prepareErr,
				stage:       StageBackendRun,
			},
			warnings:   &controlContractWarningEmitter{},
			input:      controlTestLine(ControlCancel, controlTestID(1)) + "\n",
			allowed:    []ControlKind{ControlCancel},
			wantErr:    `prepare stdin control "cancel": prepare failed`,
			wantTarget: prepareErr,
		},
		{
			name: "action error",
			handler: &controlResultHandler{
				disposition: ControlAccepted,
				action:      func() error { return actionErr },
				stage:       StageBackendRun,
			},
			warnings:   &controlContractWarningEmitter{},
			input:      controlTestLine(ControlCancel, controlTestID(1)) + "\n",
			allowed:    []ControlKind{ControlCancel},
			wantErr:    `apply stdin control "cancel": action failed`,
			wantTarget: actionErr,
		},
		{
			name: "zero disposition",
			handler: &controlResultHandler{
				disposition: ControlDispositionUnknown,
				stage:       StageBackendRun,
			},
			warnings: &controlContractWarningEmitter{},
			input:    controlTestLine(ControlCancel, controlTestID(1)) + "\n",
			allowed:  []ControlKind{ControlCancel},
			wantErr:  `prepare stdin control "cancel": disposition 0 is not supported`,
		},
		{
			name: "unknown disposition",
			handler: &controlResultHandler{
				disposition: ControlDisposition(99),
				stage:       StageBackendRun,
			},
			warnings: &controlContractWarningEmitter{},
			input:    controlTestLine(ControlCancel, controlTestID(1)) + "\n",
			allowed:  []ControlKind{ControlCancel},
			wantErr:  `prepare stdin control "cancel": disposition 99 is not supported`,
		},
		{
			name: "accepted nil action",
			handler: &controlResultHandler{
				disposition: ControlAccepted,
				stage:       StageBackendRun,
			},
			warnings: &controlContractWarningEmitter{},
			input:    controlTestLine(ControlCancel, controlTestID(1)) + "\n",
			allowed:  []ControlKind{ControlCancel},
			wantErr:  `prepare stdin control "cancel": accepted command returned nil action`,
		},
		{
			name: "not applicable with action",
			handler: &controlResultHandler{
				disposition: ControlNotApplicable,
				action:      func() error { return nil },
				stage:       StageBackendRun,
			},
			warnings: &controlContractWarningEmitter{},
			input:    controlTestLine(ControlCancel, controlTestID(1)) + "\n",
			allowed:  []ControlKind{ControlCancel},
			wantErr:  `prepare stdin control "cancel": not-applicable command returned non-nil action`,
		},
		{
			name: "unknown warning stage",
			handler: &controlResultHandler{
				disposition: ControlNotApplicable,
				stage:       Stage("future_stage"),
			},
			warnings: &controlContractWarningEmitter{},
			input:    controlTestLine(ControlCancel, controlTestID(1)) + "\n",
			allowed:  []ControlKind{ControlCancel},
			wantErr:  `emit invalid stdin control warning: unknown current stage "future_stage"`,
		},
		{
			name: "warning sink error",
			handler: &controlResultHandler{
				disposition: ControlNotApplicable,
				stage:       StageBackendRun,
			},
			warnings:   &controlContractWarningEmitter{err: warningErr},
			input:      controlTestLine(ControlCancel, controlTestID(1)) + "\n",
			allowed:    []ControlKind{ControlCancel},
			wantErr:    "emit invalid stdin control warning: warning failed",
			wantTarget: warningErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, err := NewControlReader(
				strings.NewReader(tt.input),
				tt.warnings,
				tt.handler,
				tt.allowed...,
			)
			if err != nil {
				t.Fatalf("NewControlReader() error = %v", err)
			}

			err = reader.Run(context.Background())
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("Run() error = %v, want %q", err, tt.wantErr)
			}
			if tt.wantTarget != nil && !errors.Is(err, tt.wantTarget) {
				t.Fatalf("Run() error = %v, want wrapping %v", err, tt.wantTarget)
			}
		})
	}
}

func TestControlReader_InvalidLinesWarnOnceAndRecover(t *testing.T) {
	const validCancel = `{"protocol":1,"command":"cancel","commandId":"01J00000000000000000000000"}`
	tests := []struct {
		name        string
		line        []byte
		wantReason  string
		wantDetails map[string]any
	}{
		{
			name:        "empty line",
			line:        []byte{},
			wantReason:  "empty_line",
			wantDetails: map[string]any{"reason": "empty_line", "lineBytes": 0},
		},
		{
			name:        "whitespace is invalid JSON",
			line:        []byte(" "),
			wantReason:  "invalid_json",
			wantDetails: map[string]any{"reason": "invalid_json", "lineBytes": 1},
		},
		{
			name:        "invalid UTF-8",
			line:        []byte{0xff},
			wantReason:  "invalid_utf8",
			wantDetails: map[string]any{"reason": "invalid_utf8", "lineBytes": 1},
		},
		{
			name:        "invalid JSON syntax",
			line:        []byte(`{"protocol":`),
			wantReason:  "invalid_json",
			wantDetails: map[string]any{"reason": "invalid_json", "lineBytes": 12},
		},
		{
			name:        "non object",
			line:        []byte(`[]`),
			wantReason:  "invalid_json",
			wantDetails: map[string]any{"reason": "invalid_json", "lineBytes": 2},
		},
		{
			name:        "trailing token",
			line:        []byte(`{} {}`),
			wantReason:  "invalid_json",
			wantDetails: map[string]any{"reason": "invalid_json", "lineBytes": 5},
		},
		{
			name:        "unknown field",
			line:        []byte(`{"extra":true,"protocol":1,"command":"cancel","commandId":"01J00000000000000000000000"}`),
			wantReason:  "unknown_field",
			wantDetails: map[string]any{"reason": "unknown_field", "field": "extra"},
		},
		{
			name:        "duplicate field",
			line:        []byte(`{"protocol":1,"protocol":1,"command":"cancel","commandId":"01J00000000000000000000000"}`),
			wantReason:  "duplicate_field",
			wantDetails: map[string]any{"reason": "duplicate_field", "field": "protocol"},
		},
		{
			name:        "missing protocol before invalid command type",
			line:        []byte(`{"command":1,"commandId":"01J00000000000000000000000"}`),
			wantReason:  "missing_field",
			wantDetails: map[string]any{"reason": "missing_field", "field": "protocol"},
		},
		{
			name:        "missing command",
			line:        []byte(`{"protocol":1,"commandId":"01J00000000000000000000000"}`),
			wantReason:  "missing_field",
			wantDetails: map[string]any{"reason": "missing_field", "field": "command"},
		},
		{
			name:        "missing command ID",
			line:        []byte(`{"protocol":1,"command":"cancel"}`),
			wantReason:  "missing_field",
			wantDetails: map[string]any{"reason": "missing_field", "field": "commandId"},
		},
		{
			name:        "protocol null",
			line:        []byte(`{"protocol":null,"command":"cancel","commandId":"01J00000000000000000000000"}`),
			wantReason:  "invalid_field",
			wantDetails: map[string]any{"reason": "invalid_field", "field": "protocol"},
		},
		{
			name:        "protocol wrong type before later invalid fields",
			line:        []byte(`{"protocol":"1","command":1,"commandId":null}`),
			wantReason:  "invalid_field",
			wantDetails: map[string]any{"reason": "invalid_field", "field": "protocol"},
		},
		{
			name:        "command null",
			line:        []byte(`{"protocol":1,"command":null,"commandId":"01J00000000000000000000000"}`),
			wantReason:  "invalid_field",
			wantDetails: map[string]any{"reason": "invalid_field", "field": "command"},
		},
		{
			name:        "command ID null",
			line:        []byte(`{"protocol":1,"command":"cancel","commandId":null}`),
			wantReason:  "invalid_field",
			wantDetails: map[string]any{"reason": "invalid_field", "field": "commandId"},
		},
		{
			name:        "unsupported protocol before invalid command ID",
			line:        []byte(`{"protocol":2,"command":"cancel","commandId":"bad"}`),
			wantReason:  "unsupported_protocol",
			wantDetails: map[string]any{"reason": "unsupported_protocol", "expectedProtocol": 1, "receivedProtocol": 2},
		},
		{
			name:        "non canonical command ID before unsupported command",
			line:        []byte(`{"protocol":1,"command":"pause","commandId":"01j00000000000000000000000"}`),
			wantReason:  "invalid_command_id",
			wantDetails: map[string]any{"reason": "invalid_command_id", "field": "commandId"},
		},
		{
			name:        "unsupported command",
			line:        []byte(`{"protocol":1,"command":"pause","commandId":"01J00000000000000000000001"}`),
			wantReason:  "unsupported_command",
			wantDetails: map[string]any{"reason": "unsupported_command", "command": ControlKind("pause"), "controlCommandId": "01J00000000000000000000001"},
		},
	}
	const canonicalID = "01J00000000000000000000000"
	invalidCommandIDs := []struct {
		name string
		id   string
	}{
		{name: "command ID has 25 bytes", id: canonicalID[:25]},
		{name: "command ID has 27 bytes", id: canonicalID + "0"},
		{name: "command ID first character exceeds seven", id: "8" + canonicalID[1:]},
		{name: "command ID contains I", id: canonicalID[:5] + "I" + canonicalID[6:]},
		{name: "command ID contains L", id: canonicalID[:5] + "L" + canonicalID[6:]},
		{name: "command ID contains O", id: canonicalID[:5] + "O" + canonicalID[6:]},
		{name: "command ID contains U", id: canonicalID[:5] + "U" + canonicalID[6:]},
		{name: "command ID contains lowercase", id: canonicalID[:5] + "a" + canonicalID[6:]},
	}
	for _, invalid := range invalidCommandIDs {
		tests = append(tests, struct {
			name        string
			line        []byte
			wantReason  string
			wantDetails map[string]any
		}{
			name:        invalid.name,
			line:        []byte(`{"protocol":1,"command":"cancel","commandId":"` + invalid.id + `"}`),
			wantReason:  "invalid_command_id",
			wantDetails: map[string]any{"reason": "invalid_command_id", "field": "commandId"},
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := append(append(append([]byte(nil), tt.line...), '\n'), []byte(validCancel+"\n")...)
			handler := &controlContractHandler{}
			warnings := &controlContractWarningEmitter{}
			reader, err := NewControlReader(bytes.NewReader(input), warnings, handler, ControlCancel)
			if err != nil {
				t.Fatalf("NewControlReader() error = %v", err)
			}

			if err := reader.Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if handler.actions != 1 {
				t.Fatalf("action calls = %d, want 1 after recovery", handler.actions)
			}
			if len(warnings.warnings) != 1 {
				t.Fatalf("warning count = %d, want 1: %#v", len(warnings.warnings), warnings.warnings)
			}
			assertInvalidControlWarning(t, warnings.warnings[0], tt.wantDetails)
			if got := warnings.warnings[0].Details["reason"]; got != tt.wantReason {
				t.Fatalf("warning reason = %#v, want %q", got, tt.wantReason)
			}
		})
	}
}

func TestControlReader_InvalidFieldTypesWarnAndRecover(t *testing.T) {
	const (
		validCancel = `{"protocol":1,"command":"cancel","commandId":"01J00000000000000000000000"}`
		validID     = `"01J00000000000000000000000"`
	)
	tests := []struct {
		name  string
		field string
		line  string
	}{
		{name: "protocol fractional number", field: "protocol", line: `{"protocol":1.5,"command":"cancel","commandId":` + validID + `}`},
		{name: "protocol boolean", field: "protocol", line: `{"protocol":true,"command":"cancel","commandId":` + validID + `}`},
		{name: "protocol object", field: "protocol", line: `{"protocol":{},"command":"cancel","commandId":` + validID + `}`},
		{name: "protocol array", field: "protocol", line: `{"protocol":[],"command":"cancel","commandId":` + validID + `}`},
		{name: "command number", field: "command", line: `{"protocol":1,"command":1,"commandId":` + validID + `}`},
		{name: "command boolean", field: "command", line: `{"protocol":1,"command":true,"commandId":` + validID + `}`},
		{name: "command object", field: "command", line: `{"protocol":1,"command":{},"commandId":` + validID + `}`},
		{name: "command array", field: "command", line: `{"protocol":1,"command":[],"commandId":` + validID + `}`},
		{name: "command ID number", field: "commandId", line: `{"protocol":1,"command":"cancel","commandId":1}`},
		{name: "command ID boolean", field: "commandId", line: `{"protocol":1,"command":"cancel","commandId":true}`},
		{name: "command ID object", field: "commandId", line: `{"protocol":1,"command":"cancel","commandId":{}}`},
		{name: "command ID array", field: "commandId", line: `{"protocol":1,"command":"cancel","commandId":[]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &controlContractHandler{}
			warnings := &controlContractWarningEmitter{}
			reader, err := NewControlReader(
				strings.NewReader(tt.line+"\n"+validCancel+"\n"),
				warnings,
				handler,
				ControlCancel,
			)
			if err != nil {
				t.Fatalf("NewControlReader() error = %v", err)
			}

			if err := reader.Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if handler.actions != 1 {
				t.Fatalf("action calls = %d, want 1 after recovery", handler.actions)
			}
			if len(warnings.warnings) != 1 {
				t.Fatalf("warning count = %d, want 1", len(warnings.warnings))
			}
			assertInvalidControlWarning(t, warnings.warnings[0], map[string]any{
				"reason": "invalid_field",
				"field":  tt.field,
			})
		})
	}
}

func TestControlReader_FieldDiscoveryUsesTokenOrder(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantDetails map[string]any
	}{
		{
			name:        "unknown before duplicate",
			line:        `{"extra":1,"protocol":1,"protocol":1}`,
			wantDetails: map[string]any{"reason": "unknown_field", "field": "extra"},
		},
		{
			name:        "duplicate before unknown",
			line:        `{"protocol":1,"protocol":1,"extra":1}`,
			wantDetails: map[string]any{"reason": "duplicate_field", "field": "protocol"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings := &controlContractWarningEmitter{}
			reader, err := NewControlReader(
				strings.NewReader(tt.line+"\n"),
				warnings,
				&controlContractHandler{},
				ControlCancel,
			)
			if err != nil {
				t.Fatalf("NewControlReader() error = %v", err)
			}

			if err := reader.Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if len(warnings.warnings) != 1 {
				t.Fatalf("warning count = %d, want 1", len(warnings.warnings))
			}
			assertInvalidControlWarning(t, warnings.warnings[0], tt.wantDetails)
		})
	}
}

func TestControlReader_LineLengthBoundaryAndRecovery(t *testing.T) {
	const validCancel = `{"protocol":1,"command":"cancel","commandId":"01J00000000000000000000000"}`

	t.Run("accepts exactly 4096 payload bytes", func(t *testing.T) {
		line := validCancel + strings.Repeat(" ", 4096-len(validCancel))
		handler := &controlContractHandler{}
		warnings := &controlContractWarningEmitter{}
		reader, err := NewControlReader(strings.NewReader(line+"\r\n"), warnings, handler, ControlCancel)
		if err != nil {
			t.Fatalf("NewControlReader() error = %v", err)
		}

		if err := reader.Run(context.Background()); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if handler.actions != 1 {
			t.Fatalf("action calls = %d, want 1", handler.actions)
		}
		if len(warnings.warnings) != 0 {
			t.Fatalf("warnings = %#v, want none", warnings.warnings)
		}
	})

	tests := []struct {
		name      string
		separator string
	}{
		{name: "4097 with LF", separator: "\n"},
		{name: "4097 with CRLF", separator: "\r\n"},
		{name: "overlong final line without newline", separator: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tooLong := strings.Repeat("x", 4097)
			input := tooLong + tt.separator
			wantActions := 0
			if tt.separator != "" {
				input += validCancel + "\n"
				wantActions = 1
			}
			handler := &controlContractHandler{}
			warnings := &controlContractWarningEmitter{}
			reader, err := NewControlReader(strings.NewReader(input), warnings, handler, ControlCancel)
			if err != nil {
				t.Fatalf("NewControlReader() error = %v", err)
			}

			if err := reader.Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if handler.actions != wantActions {
				t.Fatalf("action calls = %d, want %d", handler.actions, wantActions)
			}
			if len(warnings.warnings) != 1 {
				t.Fatalf("warning count = %d, want 1", len(warnings.warnings))
			}
			assertInvalidControlWarning(t, warnings.warnings[0], map[string]any{
				"reason":    "line_too_long",
				"lineBytes": 4097,
			})
		})
	}

	t.Run("length wins over UTF-8 and recovers after multiple buffer fragments", func(t *testing.T) {
		tooLong := append(bytes.Repeat([]byte{'x'}, 9000), 0xff)
		input := append(append(tooLong, '\n'), []byte(validCancel+"\n")...)
		handler := &controlContractHandler{}
		warnings := &controlContractWarningEmitter{}
		reader, err := NewControlReader(bytes.NewReader(input), warnings, handler, ControlCancel)
		if err != nil {
			t.Fatalf("NewControlReader() error = %v", err)
		}

		if err := reader.Run(context.Background()); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if handler.actions != 1 {
			t.Fatalf("action calls = %d, want 1 after recovery", handler.actions)
		}
		if len(warnings.warnings) != 1 {
			t.Fatalf("warning count = %d, want 1", len(warnings.warnings))
		}
		assertInvalidControlWarning(t, warnings.warnings[0], map[string]any{
			"reason":    "line_too_long",
			"lineBytes": 9001,
		})
	})
}

func TestControlReader_ReadErrorsReturnWithoutWarning(t *testing.T) {
	readErr := errors.New("read failed")
	warnings := &controlContractWarningEmitter{}
	reader, err := NewControlReader(
		errorControlInput{err: readErr},
		warnings,
		&controlContractHandler{},
		ControlCancel,
	)
	if err != nil {
		t.Fatalf("NewControlReader() error = %v", err)
	}

	err = reader.Run(context.Background())
	if !errors.Is(err, readErr) {
		t.Fatalf("Run() error = %v, want wrapping %v", err, readErr)
	}
	if len(warnings.warnings) != 0 {
		t.Fatalf("warnings = %#v, want none", warnings.warnings)
	}
}

type errorControlInput struct {
	err error
}

func (input errorControlInput) Read([]byte) (int, error) {
	return 0, input.err
}

func assertInvalidControlWarning(t *testing.T, got WarningEvent, wantDetails map[string]any) {
	t.Helper()
	if got.Code != string(CodeInvalidControlCommand) {
		t.Fatalf("warning code = %q, want %q", got.Code, CodeInvalidControlCommand)
	}
	if got.Stage != StageBackendRun {
		t.Fatalf("warning stage = %q, want %q", got.Stage, StageBackendRun)
	}
	if got.Message != "已忽略无效的 stdin 控制命令" {
		t.Fatalf("warning message = %q", got.Message)
	}
	if got.Retryable {
		t.Fatal("warning retryable = true, want false")
	}
	if !reflect.DeepEqual(got.Remediation, []string{string(RemediationUpdateDesktop)}) {
		t.Fatalf("warning remediation = %#v", got.Remediation)
	}
	if !reflect.DeepEqual(got.Details, wantDetails) {
		t.Fatalf("warning details = %#v, want %#v", got.Details, wantDetails)
	}
}
