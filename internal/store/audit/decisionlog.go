package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/store"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteDecisionLog 实现了 protocol.DecisionLogger。
// 所有写入请求通过 MutationBus 单写者进行序列化。
type SQLiteDecisionLog struct {
	writer *store.DatabaseWriter
}

var _ protocol.DecisionLogger = (*SQLiteDecisionLog)(nil)

// NewSQLiteDecisionLog 创建基于 SQLite MutationBus 的 DecisionLogger
func NewSQLiteDecisionLog(writer *store.DatabaseWriter) *SQLiteDecisionLog {
	return &SQLiteDecisionLog{writer: writer}
}

// AppendDecision 提交决策插入 intent 到串行总线。
func (l *SQLiteDecisionLog) AppendDecision(ctx context.Context, entry *types.DecisionLogEntry) error {
	payload, err := json.Marshal(entry)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "marshal decision log entry", err)
	}

	resultCh := make(chan error, 1)
	intent := &store.MutationIntent{
		Table:     "decision_log",
		Operation: "insert_decision", // 会触发 executeInsertDecision
		Payload:   payload,
		ResultCh:  resultCh,
	}

	if err := l.writer.Submit(ctx, intent); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "submit decision log mutation", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-resultCh:
		if err != nil {
			return fmt.Errorf("SQLiteDecisionLog.AppendDecision: %w", err)
		}
		return nil
	}
}
