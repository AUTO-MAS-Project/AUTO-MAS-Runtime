package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/AUTO-MAS-Project/AUTO-MAS-Runtime/internal/filesystem"
)

// stderrAuditor 把受控删除审计记录写入 stderr 诊断通道。
type stderrAuditor struct {
	err io.Writer
}

func (a stderrAuditor) RecordDeletion(
	ctx context.Context,
	record filesystem.DeleteAuditRecord,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.err == nil {
		return nil
	}
	_, err := fmt.Fprintf(
		a.err,
		"audit delete phase=%s kind=%s operation=%s target=%q reason=%q removed=%t partial=%t result=%s\n",
		record.Phase,
		record.Kind,
		record.OperationID,
		record.Target,
		record.Reason,
		record.Removed,
		record.Partial,
		record.Result,
	)
	return err
}
