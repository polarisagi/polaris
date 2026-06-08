package storage

import (
	"context"
	"encoding/json"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate"
)

// SQLiteDecisionLog 实现了 protocol.DecisionLogger。
// 所有写入请求通过 MutationBus 单写者进行序列化。
type SQLiteDecisionLog struct {
	writer *substrate.DatabaseWriter
}

var _ protocol.DecisionLogger = (*SQLiteDecisionLog)(nil)

// NewSQLiteDecisionLog 创建基于 SQLite MutationBus 的 DecisionLogger
func NewSQLiteDecisionLog(writer *substrate.DatabaseWriter) *SQLiteDecisionLog {
	return &SQLiteDecisionLog{writer: writer}
}

// AppendDecision 提交决策插入 intent 到串行总线。
func (l *SQLiteDecisionLog) AppendDecision(ctx context.Context, entry *protocol.DecisionLogEntry) error {
	payload, err := json.Marshal(entry)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "marshal decision log entry", err)
	}

	resultCh := make(chan error, 1)
	intent := &substrate.MutationIntent{
		Table:     "decision_log",
		Operation: "insert_decision", // 会触发 executeInsertDecision
		Payload:   payload,
		ResultCh:  resultCh,
	}

	if err := l.writer.Submit(ctx, intent); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "submit decision log mutation", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-resultCh:
		return err
	}
}
