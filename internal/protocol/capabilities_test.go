package protocol_test

import (
	"reflect"
	"slices"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestEmitterCapabilityMethodSets(t *testing.T) {
	t.Parallel()

	operationType := reflect.TypeOf((*protocol.OperationEmitter)(nil)).Elem()
	wantOperation := []string{
		"EmitError", "EmitProgress", "EmitResult", "EmitWarning",
	}
	if got := interfaceMethodNames(operationType); !reflect.DeepEqual(got, wantOperation) {
		t.Fatalf("OperationEmitter methods = %#v, want %#v", got, wantOperation)
	}

	lifecycleType := reflect.TypeOf((*protocol.LifecycleEmitter)(nil)).Elem()
	wantLifecycle := append(append([]string(nil), wantOperation...), "EmitState")
	slices.Sort(wantLifecycle)
	if got := interfaceMethodNames(lifecycleType); !reflect.DeepEqual(got, wantLifecycle) {
		t.Fatalf("LifecycleEmitter methods = %#v, want %#v", got, wantLifecycle)
	}
}

func TestOperationOnlyEmitterSatisfiesOperationEmitter(t *testing.T) {
	t.Parallel()

	var _ protocol.OperationEmitter = operationOnlyEmitter{}
}

var _ protocol.OperationEmitter = (*protocol.Emitter)(nil)
var _ protocol.LifecycleEmitter = (*protocol.Emitter)(nil)

type operationOnlyEmitter struct{}

func (operationOnlyEmitter) EmitProgress(protocol.ProgressEvent) error { return nil }

func (operationOnlyEmitter) EmitWarning(protocol.WarningEvent) error { return nil }

func (operationOnlyEmitter) EmitError(protocol.ErrorEvent) error { return nil }

func (operationOnlyEmitter) EmitResult(protocol.ResultEvent) error { return nil }

func interfaceMethodNames(interfaceType reflect.Type) []string {
	methodNames := make([]string, interfaceType.NumMethod())
	for index := range methodNames {
		methodNames[index] = interfaceType.Method(index).Name
	}
	slices.Sort(methodNames)
	return methodNames
}
