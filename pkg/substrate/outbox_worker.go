package substrate

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate/observability"
)

var ErrVersionStale = errors.New("outbox: version stale")

// OutboxWorker — 跨引擎投递 Worker。
// 消费 M2 outbox 表，将投影写入目标引擎（[Storage-SQLite] / [Storage-SurrealDB-Core]）。
// 架构文档: docs/arch/M02-Storage-Fabric.md §2.5

type OutboxWorker struct {
	db           *sql.DB
	handlers     map[string]OutboxHandler // TargetEngine → HandlerFunc
	versionCheck map[string]func(context.Context, *OutboxRecord) (int64, error)
	pollInterval int64 // seconds，5s 默认
	maxRetries   int   // 3
}

// NewOutboxWorker 创建 OutboxWorker，db 必须非 nil（fail-fast）。
func NewOutboxWorker(db *sql.DB, pollInterval int64, maxRetries int) *OutboxWorker {
	if pollInterval <= 0 {
		pollInterval = 5
	}
	if maxRetries <= 0 {
		maxRetries = 3
	}
	return &OutboxWorker{
		db:           db,
		handlers:     make(map[string]OutboxHandler),
		versionCheck: make(map[string]func(context.Context, *OutboxRecord) (int64, error)),
		pollInterval: pollInterval,
		maxRetries:   maxRetries,
	}
}

// OutboxHandler 处理单个 Outbox 任务。
type OutboxHandler func(ctx context.Context, record *OutboxRecord) error

// OutboxRecord 单条 Outbox 记录。
type OutboxRecord struct {
	ID                 int64
	TargetEngine       string
	Operation          string
	Scope              string
	Payload            []byte
	IdempotencyKey     protocol.IdempotencyKey
	Version            int64
	Attempts           int
	CrashRecoveryCount int
}

// RegisterHandler 注册 Outbox 任务处理器。可选传入版本检查器以支持高水位拦截。
func (w *OutboxWorker) RegisterHandler(taskType string, handler OutboxHandler, checker ...func(context.Context, *OutboxRecord) (int64, error)) {
	w.handlers[taskType] = handler
	if len(checker) > 0 {
		w.versionCheck[taskType] = checker[0]
	}
}

// Write 实现 protocol.OutboxWriter 接口，提供同步写入 Outbox 表的能力。
func (w *OutboxWorker) Write(ctx context.Context, entry protocol.OutboxEntry) error {
	if w.db == nil {
		return perrors.New(perrors.CodeInternal, "outbox db is nil")
	}
	const q = `INSERT INTO outbox (target_engine, operation, scope, payload, idempotency_key, status, created_at) VALUES (?, ?, ?, ?, ?, 'pending', ?)`
	now := time.Now().UnixMilli()
	_, err := w.db.ExecContext(ctx, q, entry.TargetEngine, entry.Operation, entry.Scope, entry.Payload, entry.IdempotencyKey, now)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "failed to write outbox", err)
	}
	return nil
}

// FetchBatch 读取待处理 Outbox 记录。
// 主查询: WHERE id > cursor AND status IN ('pending','failed')
//
//	AND (next_retry_at IS NULL OR next_retry_at <= now) ORDER BY id LIMIT batchSize
//
// 补充查询 (cursor>0): WHERE id <= cursor AND status='failed' ORDER BY id LIMIT batchSize/4
func (w *OutboxWorker) FetchBatch(ctx context.Context, cursor int64, batchSize int) ([]*OutboxRecord, error) {
	if w.db == nil {
		return nil, perrors.New(perrors.CodeInternal, "outbox db is nil")
	}
	now := time.Now().UnixMilli()

	const mainQ = `
		SELECT id, target_engine, operation, scope, payload, idempotency_key,
		       attempts, crash_recovery_count
		FROM outbox
		WHERE id > ? AND status IN ('pending','failed')
		  AND (next_retry_at IS NULL OR next_retry_at <= ?)
		ORDER BY id LIMIT ?`

	rows, err := w.db.QueryContext(ctx, mainQ, cursor, now, batchSize)
	if err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "outbox fetch failed", err)
	}
	defer rows.Close()

	records, err := scanOutboxRows(rows)
	if err != nil {
		return nil, err
	}

	// 补充查询: cursor 以前遗漏的失败记录
	if cursor > 0 && batchSize > 4 {
		suppLimit := batchSize / 4
		const suppQ = `
			SELECT id, target_engine, operation, scope, payload, idempotency_key,
			       attempts, crash_recovery_count
			FROM outbox
			WHERE id <= ? AND status = 'failed'
			  AND (next_retry_at IS NULL OR next_retry_at <= ?)
			ORDER BY id LIMIT ?`
		suppRows, suppErr := w.db.QueryContext(ctx, suppQ, cursor, now, suppLimit)
		if suppErr == nil {
			defer suppRows.Close()
			if supp, scanErr := scanOutboxRows(suppRows); scanErr == nil {
				records = append(records, supp...)
			}
		}
	}
	return records, nil
}

func scanOutboxRows(rows *sql.Rows) ([]*OutboxRecord, error) {
	var records []*OutboxRecord
	for rows.Next() {
		r := &OutboxRecord{}
		var ikey string
		if err := rows.Scan(
			&r.ID, &r.TargetEngine, &r.Operation, &r.Scope, &r.Payload,
			&ikey, &r.Attempts, &r.CrashRecoveryCount,
		); err != nil {
			return nil, perrors.Wrap(perrors.CodeInternal, "outbox scan failed", err)
		}
		r.IdempotencyKey = protocol.IdempotencyKey(ikey)
		parts := strings.Split(ikey, ":")
		if len(parts) >= 5 {
			if v, err := strconv.ParseInt(parts[4], 10, 64); err == nil {
				r.Version = v
			}
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "outbox rows error", err)
	}
	return records, nil
}

// Run 启动增量消费主循环，阻塞直到 ctx 取消。
//
// 设计约束（HE-Rule-6 State-in-DB）：
//   - cursor 持久化到 sys_config 表（键 "outbox_cursor"），重启后从 DB 恢复，防止漏消费。
//   - 每批处理完成后原子 CAS 更新 cursor，保证 Exactly-Once 推进。
//   - 毒丸记录（crash_recovery_count ≥ 3）直接标记 dead，不再重试。
//   - 处于 ReplayMode 时跳过所有副作用（只消费，不触发 handler）。
func (w *OutboxWorker) Run(ctx context.Context) error {
	// 从 DB 恢复 cursor（崩溃重启场景）
	cursor := w.loadCursor(ctx)

	ticker := time.NewTicker(time.Duration(w.pollInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			newCursor, err := w.processBatch(ctx, cursor, 50)
			if err != nil {
				// 非致命：记录并继续，防止单批失败中断整个 worker
				continue
			}
			if newCursor > cursor {
				cursor = newCursor
				w.saveCursor(ctx, cursor)
			}
		}
	}
}

// processBatch 获取并处理一批记录，返回处理完成后的最大 ID（新 cursor）。
func (w *OutboxWorker) processBatch(ctx context.Context, cursor int64, batchSize int) (int64, error) {
	records, err := w.FetchBatch(ctx, cursor, batchSize)
	if err != nil {
		return cursor, err
	}

	maxID := cursor
	for _, r := range records {
		_ = w.processAndMark(ctx, r) // 单条失败不中断批次
		if r.ID > maxID {
			maxID = r.ID
		}
	}
	return maxID, nil
}

// processAndMark 处理记录并更新状态（done / failed + 指数退避）。
func (w *OutboxWorker) processAndMark(ctx context.Context, record *OutboxRecord) error {
	now := time.Now().UnixMilli()

	// Atomically mark as processing to prevent concurrent worker pick-up
	res, err := w.db.ExecContext(ctx,
		"UPDATE outbox SET status='processing', updated_at=? WHERE id=? AND status IN ('pending', 'failed')",
		now, record.ID)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return nil // Grabbed by another worker, simply skip
	}

	err = w.Process(ctx, record)
	now = time.Now().UnixMilli()

	if err == nil {
		_, _ = w.db.ExecContext(ctx,
			"UPDATE outbox SET status='done', processed_at=? WHERE id=?",
			now, record.ID)
		return nil
	}

	if errors.Is(err, ErrVersionStale) {
		_, _ = w.db.ExecContext(ctx,
			"UPDATE outbox SET status='skipped', processed_at=? WHERE id=?",
			now, record.ID)
		return err
	}

	newAttempts := record.Attempts + 1
	if newAttempts >= w.maxRetries || record.CrashRecoveryCount >= 3 {
		_, _ = w.db.ExecContext(ctx,
			"UPDATE outbox SET status='dead', attempts=?, processed_at=?, crash_recovery_count=crash_recovery_count+1 WHERE id=?",
			newAttempts, now, record.ID)
		observability.GlobalOutboxDeadLetterTotal.Add(1)
		slog.Error("outbox message dead", "id", record.ID, "target", record.TargetEngine, "attempts", newAttempts, "crash", record.CrashRecoveryCount)
	} else {
		// 指数退避：2^attempt × 5s
		backoffMs := (int64(1) << newAttempts) * 5000
		nextRetry := now + backoffMs
		_, _ = w.db.ExecContext(ctx,
			"UPDATE outbox SET status='failed', attempts=?, next_retry_at=?, crash_recovery_count=crash_recovery_count+1 WHERE id=?",
			newAttempts, nextRetry, record.ID)
	}
	return err
}

// loadCursor 从 sys_config 读取持久化的消费游标。
func (w *OutboxWorker) loadCursor(ctx context.Context) int64 {
	var cursor int64
	row := w.db.QueryRowContext(ctx,
		"SELECT CAST(value AS INTEGER) FROM sys_config WHERE key='outbox_cursor' LIMIT 1")
	_ = row.Scan(&cursor)
	return cursor
}

// saveCursor 原子更新消费游标到 sys_config。
func (w *OutboxWorker) saveCursor(ctx context.Context, cursor int64) {
	_, _ = w.db.ExecContext(ctx,
		"INSERT INTO sys_config(key,value) VALUES('outbox_cursor',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		cursor)
}

// Process 处理单条 Outbox 记录。
// 版本高水位拦截: existing_version >= incoming_version → 丢弃 + ErrVersionStale
// Poison Pill: crash_recovery_count >= 3 → 直接标记 dead
func (w *OutboxWorker) Process(ctx context.Context, record *OutboxRecord) error {
	// 处于重放模式时物理切断外部副作用
	if protocol.IsReplaying() {
		return nil
	}

	if record.CrashRecoveryCount >= 3 {
		observability.GlobalOutboxDeadLetterTotal.Add(1)
		slog.Error("outbox message dead (poison pill)", "id", record.ID, "target", record.TargetEngine)
		return nil
	}

	if check, ok := w.versionCheck[record.TargetEngine]; ok && check != nil {
		existingVersion, err := check(ctx, record)
		if err == nil && existingVersion >= record.Version {
			return ErrVersionStale
		}
	}

	handler, ok := w.handlers[record.TargetEngine]
	if !ok {
		return nil
	}
	return handler(ctx, record)
}
