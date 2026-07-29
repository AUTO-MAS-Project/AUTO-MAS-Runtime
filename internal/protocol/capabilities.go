package protocol

// OperationEmitter 发射一次性操作允许产生的事件。
type OperationEmitter interface {
	EmitProgress(ProgressEvent) error
	EmitWarning(WarningEvent) error
	EmitError(ErrorEvent) error
	EmitResult(ResultEvent) error
}

// LifecycleEmitter 在操作事件之外还可发射生命周期迁移或只读状态快照。
type LifecycleEmitter interface {
	OperationEmitter
	EmitState(StateEvent) error
}
