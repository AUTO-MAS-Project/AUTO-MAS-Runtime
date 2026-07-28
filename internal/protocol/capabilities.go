package protocol

// OperationEmitter emits events for a one-time operation.
type OperationEmitter interface {
	EmitProgress(ProgressEvent) error
	EmitWarning(WarningEvent) error
	EmitError(ErrorEvent) error
	EmitResult(ResultEvent) error
}

// LifecycleEmitter emits operation events plus lifecycle transitions or
// read-only state snapshots.
type LifecycleEmitter interface {
	OperationEmitter
	EmitState(StateEvent) error
}
