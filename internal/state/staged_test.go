package state

import (
	"strings"
	"testing"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/protocol"
)

func TestTransaction_StagedCommitValidation(t *testing.T) {
	value := validTransactionState(TransactionUpdate)
	value.Command = "workspace stage"
	value.Stage = protocol.StageWorkspaceVerify
	value.TargetCommit = strings.Repeat("b", 40)
	value.BaseCommit = strings.Repeat("a", 40)
	if err := ValidateTransaction(TransactionUpdate, value); err != nil {
		t.Fatal(err)
	}
	value.TargetCommit = "invalid"
	if err := ValidateTransaction(TransactionUpdate, value); err == nil {
		t.Fatal("invalid target commit accepted")
	}
	value.TargetCommit = ""
	if err := ValidateTransaction(TransactionUpdate, value); err == nil {
		t.Fatal("verified stage without target commit accepted")
	}
}
