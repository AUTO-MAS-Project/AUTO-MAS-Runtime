package protocol

import (
	"encoding/json"
	"testing"
)

type controlContractHandler struct{}

func (*controlContractHandler) PrepareControl(ControlCommand) (ControlDisposition, ControlAction, error) {
	return ControlAccepted, func() error { return nil }, nil
}

func (*controlContractHandler) CurrentControlStage() Stage {
	return Stage("preflight")
}

type controlContractWarningEmitter struct{}

func (*controlContractWarningEmitter) EmitWarning(WarningEvent) error {
	return nil
}

var (
	_ ControlHandler        = (*controlContractHandler)(nil)
	_ ControlWarningEmitter = (*controlContractWarningEmitter)(nil)
)

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
