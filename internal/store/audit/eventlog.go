package audit

import (
	"context"
	"fmt"

	"github.com/polarisagi/polaris/internal/store"

	"google.golang.org/protobuf/proto"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/pb"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SQLiteEventLog 实现了 protocol.EventLogger。
// 所有写入请求通过 MutationBus 单写者进行序列化，以保证全局时序 (offset)。
type SQLiteEventLog struct {
	writer *store.DatabaseWriter
}

var _ protocol.EventLogger = (*SQLiteEventLog)(nil)

// NewSQLiteEventLog 创建基于 SQLite MutationBus 的 EventLogger
func NewSQLiteEventLog(writer *store.DatabaseWriter) *SQLiteEventLog {
	return &SQLiteEventLog{writer: writer}
}

// AppendEvent 提交插入 intent 到串行总线。
func (l *SQLiteEventLog) AppendEvent(ctx context.Context, ev *pb.Event) error {
	payload, err := proto.Marshal(ev)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "marshal event", err)
	}

	resultCh := make(chan error, 1)
	intent := &store.MutationIntent{
		Table:     "events",
		Operation: "insert_event", // 会触发 executeInsertEvent
		Payload:   payload,
		ResultCh:  resultCh,
	}

	if err := l.writer.Submit(ctx, intent); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "submit event log mutation", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-resultCh:
		if err != nil {
			return fmt.Errorf("SQLiteEventLog.AppendEvent: %w", err)
		}
		return nil
	}
}
