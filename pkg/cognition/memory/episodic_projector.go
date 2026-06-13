package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate"
)

// maxEpisodicContent episodic_events.content 最大字节数（约 500 token × 4 bytes）。
// 超限截取前 2048 字节，剩余部分通过 log_ref 引用；BM25/FTS 仍可索引摘要。
const maxEpisodicContent = 2048

// EpisodicProjectorHandler 消费 outbox 中 target_engine="episodic" 的记录，
// 将事件投影写入 episodic_events 表（M05 §3.1 派生投影表）。
// 注意：本 handler 为幂等操作（INSERT OR IGNORE），重放安全。
func EpisodicProjectorHandler(db *sql.DB) substrate.OutboxHandler {
	return func(ctx context.Context, record *substrate.OutboxRecord) error {
		var ev protocol.Event
		if err := json.Unmarshal(record.Payload, &ev); err != nil {
			return fmt.Errorf("episodic projector: unmarshal event: %w", err)
		}

		content := string(ev.Payload)
		if len(content) > maxEpisodicContent {
			// 超限截断：保留可搜索摘要，log_ref 由上游写入时已处理
			content = content[:maxEpisodicContent]
		}

		now := time.Now().UnixMilli()
		sessionID := ev.TaskID // TaskID 即 SessionID（M4 约定）
		if sessionID == "" {
			sessionID = "unknown"
		}

		// seq 使用 outbox record.ID 保证单调递增（同 session 内唯一）
		var maxSeq int64
		_ = db.QueryRowContext(ctx,
			"SELECT COALESCE(MAX(seq), 0) FROM episodic_events WHERE session_id = ?",
			sessionID,
		).Scan(&maxSeq)

		cold := 0
		if maxSeq > 1000 && record.ID < maxSeq-1000 {
			cold = 1
		}

		_, err := db.ExecContext(ctx, `
            INSERT OR IGNORE INTO episodic_events
                (session_id, seq, timestamp, event_type, source, content,
                 salience, decay_weight, occurred_at, embed_model_version, event_uuid, cold)
            VALUES (?, ?, ?, ?, ?, ?, 0.5, 1.0, ?, '', ?, ?)`,
			sessionID,
			record.ID,       // seq
			now,             // timestamp
			string(ev.Type), // event_type
			"agent",         // source
			content,         // content（已截断）
			now,             // occurred_at
			ev.ID,           // event_uuid（供 SurrealDB VecUpsert 使用）
			cold,
		)
		return err
	}
}
