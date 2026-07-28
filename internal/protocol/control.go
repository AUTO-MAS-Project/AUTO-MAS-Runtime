package protocol

// ControlKind identifies a stdin control command.
type ControlKind string

const (
	ControlCancel   ControlKind = "cancel"
	ControlShutdown ControlKind = "shutdown"
	ControlStatus   ControlKind = "status"
)

// ControlCommand is a versioned stdin control request.
type ControlCommand struct {
	Protocol  int         `json:"protocol"`
	Command   ControlKind `json:"command"`
	CommandID string      `json:"commandId"`
}

// ControlDisposition reports whether a prepared command can be accepted.
type ControlDisposition uint8

const (
	ControlDispositionUnknown ControlDisposition = iota
	ControlAccepted
	ControlNotApplicable
)

// ControlAction commits an accepted control command.
type ControlAction func() error

// ControlHandler prepares control commands without applying their side effects.
type ControlHandler interface {
	PrepareControl(ControlCommand) (ControlDisposition, ControlAction, error)
	CurrentControlStage() Stage
}

// ControlWarningEmitter emits warnings caused by invalid control input.
type ControlWarningEmitter interface {
	EmitWarning(WarningEvent) error
}

// WithControlCommandID copies details and associates them with a control command.
func WithControlCommandID(details map[string]any, commandID string) map[string]any {
	copied := make(map[string]any, len(details)+1)
	for key, value := range details {
		copied[key] = value
	}
	copied["controlCommandId"] = commandID
	return copied
}
