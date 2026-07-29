package protocol_test

import (
	"reflect"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestErrorDefinitions_MutexOperationFailed(t *testing.T) {
	t.Parallel()

	got, ok := protocol.LookupErrorDefinition(
		protocol.CodeMutexOperationFailed,
	)
	if !ok {
		t.Fatal("LookupErrorDefinition() not found")
	}
	want := protocol.ErrorDefinition{
		Code:      protocol.CodeMutexOperationFailed,
		ExitCode:  protocol.ExitCodeOperationConflict,
		Retryable: true,
		Remediation: []protocol.Remediation{
			protocol.RemediationRetry,
			protocol.RemediationRunDoctor,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LookupErrorDefinition() = %#v, want %#v", got, want)
	}
}
