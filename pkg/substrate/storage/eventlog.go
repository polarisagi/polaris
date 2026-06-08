package storage

import (
	"context"

	"google.golang.org/protobuf/proto"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/pb"
	"github.com/polarisagi/polaris/pkg/substrate"
)

// SQLiteEventLog 实现了 protocol.EventLogger。
// 所有写入请求通过 MutationBus 单写者进行序列化，以保证全局时序 (offset)。
type SQLiteEventLog struct {
	writer *substrate.DatabaseWriter
}

var _ protocol.EventLogger = (*SQLiteEventLog)(nil)

// NewSQLiteEventLog 创建基于 SQLite MutationBus 的 EventLogger
func NewSQLiteEventLog(writer *substrate.DatabaseWriter) *SQLiteEventLog {
	return &SQLiteEventLog{writer: writer}
}

// AppendEvent 提交插入 intent 到串行总线。
func (l *SQLiteEventLog) AppendEvent(ctx context.Context, ev *pb.Event) error {
	payload, err := proto.Marshal(ev)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "marshal event", err)
	}

	resultCh := make(chan error, 1)
	intent := &substrate.MutationIntent{
		Table:     "events",
		Operation: "insert_event", // 会触发 executeInsertEvent
		Payload:   payload,
		ResultCh:  resultCh,
	}

	if err := l.writer.Submit(ctx, intent); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "submit event log mutation", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-resultCh:
		return err
	}
}
