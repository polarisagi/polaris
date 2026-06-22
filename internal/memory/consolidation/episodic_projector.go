package consolidation

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// maxEpisodicContent episodic_events.content 最大字节数（约 500 token × 4 bytes）。
// 超限截取前 2048 字节，剩余部分通过 log_ref 引用；BM25/FTS 仍可索引摘要。
const maxEpisodicContent = 2048

// EpisodicProjectorHandler 消费 outbox 中 target_engine="episodic" 的记录，
// 将事件投影写入 episodic_events 表（M05 §3.1 派生投影表）。
// 注意：本 handler 为幂等操作（INSERT OR IGNORE），重放安全。
func EpisodicProjectorHandler(db protocol.SQLQuerier, encKey []byte) store.OutboxHandler {
	return func(ctx context.Context, record *store.OutboxRecord) error {
		var ev types.Event
		if err := json.Unmarshal(record.Payload, &ev); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "episodic projector: unmarshal event", err)
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

		archived := 0
		if maxSeq > 1000 && record.ID < maxSeq-1000 {
			archived = 1
		}

		rs := string(ev.ReasoningState)
		if rs != "" {
			var errEnc error
			rs, errEnc = encryptField(encKey, rs)
			if errEnc != nil {
				return apperr.Wrap(apperr.CodeInternal, "episodic projector: reasoning_state encryption failed", errEnc)
			}
		}

		_, err := db.ExecContext(ctx, `
            INSERT OR IGNORE INTO episodic_events
                (session_id, seq, timestamp, event_type, source, content,
                 salience, decay_weight, occurred_at, embed_model_version, event_uuid, archived, reasoning_state)
            VALUES (?, ?, ?, ?, ?, ?, 0.5, 1.0, ?, '', ?, ?, ?)`,
			sessionID,
			record.ID,       // seq
			now,             // timestamp
			string(ev.Type), // event_type
			"agent",         // source
			content,         // content（已截断）
			now,             // occurred_at
			ev.ID,           // event_uuid（供 SurrealDB VecUpsert 使用）
			archived,
			rs,
		)
		if err != nil {
			return fmt.Errorf("EpisodicProjectorHandler: %w", err)
		}
		return nil
	}
}

func encryptField(key []byte, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if len(key) == 0 {
		return "", apperr.New(apperr.CodeInvalidInput, "encryption key is missing")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("encryptField: %w", err)
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("encryptField: %w", err)
	}
	nonce := make([]byte, aesgcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("encryptField: %w", err)
	}
	ciphertext := aesgcm.Seal(nil, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(append(nonce, ciphertext...)), nil
}
