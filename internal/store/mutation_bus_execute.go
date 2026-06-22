package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/pkg/types"

	"google.golang.org/protobuf/proto"

	"github.com/polarisagi/polaris/internal/protocol/pb"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// validateTable 检查表名是否在白名单内；防止 SQL 注入。
func validateTable(table string) error {
	switch table {
	case "tasks", "events", "workspace_vfs", "episodic_memory", "semantic_memory",
		"knowledge_nodes", "knowledge_edges", "entities", "rag_chunks",
		"plugin_marketplaces", "preferences", "decision_log", "skills",
		"self_improve", "outbox", "extension_instances", "notes",
		"reflection_memory", "apps":
		return nil
	default:
		return apperr.New(apperr.CodeInvalidInput,
			fmt.Sprintf("mutation_bus: table %q not in allowedTables whitelist", table))
	}
}

// executeInsertEvent 执行针对 events 表的专门插入，含 M11 hash chain 计算。
// intent.Payload 必须是 pb.Event 的序列化字节。
func (dw *DatabaseWriter) executeInsertEvent(tx *sql.Tx, intent *MutationIntent) error {
	var ev pb.Event
	if err := proto.Unmarshal(intent.Payload, &ev); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "unmarshal pb.Event", err)
	}

	// 读取链上最新 hash（同 tx 内已插入的事件对此查询可见，保证批内链式连续）
	var prevHash sql.NullString
	_ = tx.QueryRow(`SELECT hash FROM events ORDER BY offset DESC LIMIT 1`).Scan(&prevHash)

	// SHA-256(id || topic || actor || type || payload || prev_hash)
	h := sha256.New()
	h.Write([]byte(ev.Id))
	h.Write([]byte(ev.Topic))
	h.Write([]byte(ev.Actor))
	h.Write([]byte(ev.Type))
	h.Write(ev.Payload)
	if prevHash.Valid {
		h.Write([]byte(prevHash.String))
	}
	currentHash := hex.EncodeToString(h.Sum(nil))

	query := `INSERT INTO events (
		id, topic, actor, type, payload, idempotency_key,
		occurred_at, durative_group_id, created_at, prev_hash, hash
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	var idempotencyKey any
	if ev.IdempotencyKey != "" {
		idempotencyKey = ev.IdempotencyKey
	}
	var durativeGroupID any
	if ev.DurativeGroupId != "" {
		durativeGroupID = ev.DurativeGroupId
	}
	var prevHashVal any
	if prevHash.Valid {
		prevHashVal = prevHash.String
	}

	_, err := tx.Exec(query,
		ev.Id,
		ev.Topic,
		ev.Actor,
		ev.Type,
		ev.Payload,
		idempotencyKey,
		ev.OccurredAt,
		durativeGroupID,
		ev.CreatedAt,
		prevHashVal,
		currentHash,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "DatabaseWriter.executeInsertEvent", err)
	}
	return nil
}

// executeInsertDecision 执行针对 decision_log 表的专门插入。
// intent.Payload 必须是 types.DecisionLogEntry 的 JSON 序列化字节。
func (dw *DatabaseWriter) executeInsertDecision(tx *sql.Tx, intent *MutationIntent) error {
	var entry types.DecisionLogEntry
	if err := json.Unmarshal(intent.Payload, &entry); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "unmarshal DecisionLogEntry", err)
	}

	query := `INSERT INTO decision_log (
		timestamp, session_id, agent_id, decision_type, context, choice, alternatives, reason, outcome
	) VALUES (strftime('%s','now') * 1000, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := tx.Exec(query,
		entry.SessionID,
		entry.AgentID,
		entry.DecisionType,
		entry.Context,
		entry.Choice,
		entry.Alternatives,
		entry.Reason,
		entry.Outcome,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "DatabaseWriter.executeInsertDecision", err)
	}
	return nil
}

type resultRec struct {
	intent *MutationIntent
	err    error
}

// flushBatch 批量写入 SQLite。
//
// 执行顺序:
// 0. CompositeGroupID 收集: 同 GroupID 未到齐且 2*ticker=20ms 内 → 留 batch 等下个 ticker；超时 → ErrCompositeIncomplete
// 1. 租约二次校验: TaskID 非空 → leaseChecker.Verify → 失效 → ErrStaleLease
// 2. BEGIN IMMEDIATE → defer ROLLBACK（保证异常回滚）
// 3. 逐条执行: 乐观锁 Version 校验，结果延后收集
// 4. COMMIT 前 ctx.Done() 二次检查
// 5. COMMIT → 成功后统一通知各 intent ResultCh
func (dw *DatabaseWriter) flushBatch(ctx context.Context) error { //nolint:gocyclo
	if len(dw.batch) == 0 {
		return nil
	}

	dw.mu.Lock()
	defer dw.mu.Unlock()

	batch := dw.batch
	dw.batch = make([]*MutationIntent, 0, MaxBatchSize)

	// 步骤 0: CompositeGroupID 收集与超时检查
	compositeGroups := make(map[string][]*MutationIntent)
	pendingComposite := make(map[string]bool)

	for _, intent := range batch {
		if intent.CompositeGroupID != "" {
			compositeGroups[intent.CompositeGroupID] = append(compositeGroups[intent.CompositeGroupID], intent)
			pendingComposite[intent.CompositeGroupID] = true
		}
	}

	// 步骤 1: 租约二次校验
	validBatch := make([]*MutationIntent, 0, len(batch))
	for _, intent := range batch {
		if intent.TaskID != "" && intent.AgentID != "" {
			if dw.leaseChecker != nil && !dw.leaseChecker.Verify(intent.TaskID, intent.AgentID, intent.ClaimedVersion) {
				if intent.ResultCh != nil {
					intent.ResultCh <- ErrStaleLease
				}
				continue
			}
		}
		validBatch = append(validBatch, intent)

		if intent.CompositeGroupID != "" && intent.TaskID != "" {
			if dw.leaseChecker != nil && !dw.leaseChecker.Verify(intent.TaskID, intent.AgentID, intent.ClaimedVersion) {
				pendingComposite[intent.CompositeGroupID] = false
			}
		}
	}

	if len(validBatch) == 0 {
		return nil
	}

	// 步骤 2: BEGIN IMMEDIATE
	tx, err := dw.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		dw.failAll(validBatch, apperr.Wrap(apperr.CodeInternal, "BEGIN IMMEDIATE failed", err))
		return apperr.Wrap(apperr.CodeInternal, "DatabaseWriter.flushBatch", err)
	}
	defer func() { _ = tx.Rollback() }() // 异常回滚; COMMIT 后 Rollback 为 no-op

	// 步骤 3: 逐条执行，结果延后收集（防止半途通知成功后又整批回滚）
	var results []resultRec //nolint:prealloc

	for _, intent := range validBatch {
		if intent.CompositeGroupID != "" && !pendingComposite[intent.CompositeGroupID] {
			results = append(results, resultRec{intent, ErrStaleLease})
			continue
		}

		switch intent.Operation {
		case "insert":
			err = dw.executeInsert(tx, intent)
		case "upsert":
			err = dw.executeUpsert(tx, intent)
		case "delete":
			err = dw.executeDelete(tx, intent)
		case "insert_event":
			err = dw.executeInsertEvent(tx, intent)
		case "insert_decision":
			err = dw.executeInsertDecision(tx, intent)
		default:
			err = apperr.New(apperr.CodeInternal, fmt.Sprintf("unknown operation: %s", intent.Operation))
		}

		if err != nil {
			// ErrStaleLease 是预期结果，不触发回滚——仅记录该 intent 结果，继续处理后续
			if errors.Is(err, ErrStaleLease) {
				results = append(results, resultRec{intent, ErrStaleLease})
				continue
			}
			// 其他错误（DB 故障、未知操作）→ ROLLBACK 全部
			dw.failAll(validBatch, err)
			return apperr.Wrap(apperr.CodeInternal, "DatabaseWriter.flushBatch", err)
		}
		results = append(results, resultRec{intent, nil})
	}

	// 步骤 4: COMMIT 前 ctx 检查
	select {
	case <-ctx.Done():
		dw.failAll(validBatch, ctx.Err())
		return ctx.Err()
	default:
	}

	// 步骤 5: COMMIT
	if err := tx.Commit(); err != nil {
		dw.failAll(validBatch, apperr.Wrap(apperr.CodeInternal, "COMMIT failed", err))
		return apperr.Wrap(apperr.CodeInternal, "DatabaseWriter.flushBatch", err)
	}

	// COMMIT 成功后统一通知各 intent ResultCh
	for _, r := range results {
		if r.intent.ResultCh != nil {
			r.intent.ResultCh <- r.err
		}
	}

	return nil
}

// executeInsert 执行 INSERT，含乐观锁 Version 校验。
func (dw *DatabaseWriter) executeInsert(tx *sql.Tx, intent *MutationIntent) error {
	// 表名白名单校验，防止 fmt.Sprintf 拼接 SQL 时被注入
	if err := validateTable(intent.Table); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "DatabaseWriter.executeInsert", err)
	}
	if intent.ClaimedVersion > 0 {
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE id = ? AND version = ?", intent.Table)
		var count int
		if err := tx.QueryRow(query, string(intent.Key), intent.ClaimedVersion-1).Scan(&count); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "version check", err)
		}
		if count == 0 {
			return ErrStaleLease
		}
	}

	query := fmt.Sprintf("INSERT OR REPLACE INTO %s (id, payload, version, updated_at) VALUES (?, ?, ?, datetime('now'))", intent.Table)
	_, err := tx.Exec(query, string(intent.Key), string(intent.Payload), intent.ClaimedVersion)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "DatabaseWriter.executeInsert", err)
	}
	return nil
}

// executeUpsert 执行 UPSERT，含乐观锁 Version 校验。
func (dw *DatabaseWriter) executeUpsert(tx *sql.Tx, intent *MutationIntent) error {
	// 表名白名单校验，防止 fmt.Sprintf 拼接 SQL 时被注入
	if err := validateTable(intent.Table); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "DatabaseWriter.executeUpsert", err)
	}
	if intent.ClaimedVersion > 0 { //nolint:nestif
		query := fmt.Sprintf("UPDATE %s SET payload = ?, version = ?, updated_at = datetime('now') WHERE id = ? AND version = ?", intent.Table)
		result, err := tx.Exec(query, string(intent.Payload), intent.ClaimedVersion, string(intent.Key), intent.ClaimedVersion-1)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "upsert with version", err)
		}
		rows, _ := result.RowsAffected()
		if rows == 0 {
			// 不存在 oldVersion 匹配行 → INSERT 新行
			insertQuery := fmt.Sprintf("INSERT INTO %s (id, payload, version, updated_at) VALUES (?, ?, ?, datetime('now'))", intent.Table)
			_, err = tx.Exec(insertQuery, string(intent.Key), string(intent.Payload), intent.ClaimedVersion)
			if err != nil {
				// INSERT 失败可能是并发冲突（UNIQUE constraint）→ 重试一次 UPDATE
				if strings.Contains(err.Error(), "UNIQUE constraint") {
					query2 := fmt.Sprintf("UPDATE %s SET payload = ?, version = ?, updated_at = datetime('now') WHERE id = ? AND version = ?", intent.Table)
					result2, err2 := tx.Exec(query2, string(intent.Payload), intent.ClaimedVersion, string(intent.Key), intent.ClaimedVersion-1)
					if err2 != nil {
						return apperr.Wrap(apperr.CodeInternal, "upsert retry", err2)
					}
					rows2, _ := result2.RowsAffected()
					if rows2 == 0 {
						return ErrStaleLease
					}
					return nil
				}
				return apperr.Wrap(apperr.CodeInternal, "insert", err)
			}
		}
		return nil
	}

	// 无版本控制 → 简单 UPSERT
	query := fmt.Sprintf("INSERT OR REPLACE INTO %s (id, payload, updated_at) VALUES (?, ?, datetime('now'))", intent.Table)
	_, err := tx.Exec(query, string(intent.Key), string(intent.Payload))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "DatabaseWriter.executeUpsert", err)
	}
	return nil
}

// executeDelete 执行 DELETE。
func (dw *DatabaseWriter) executeDelete(tx *sql.Tx, intent *MutationIntent) error {
	// 表名白名单校验，防止 fmt.Sprintf 拼接 SQL 时被注入
	if err := validateTable(intent.Table); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "DatabaseWriter.executeDelete", err)
	}
	query := fmt.Sprintf("DELETE FROM %s WHERE id = ?", intent.Table)
	_, err := tx.Exec(query, string(intent.Key))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "DatabaseWriter.executeDelete", err)
	}
	return nil
}

// failAll 批量失败通知。
func (dw *DatabaseWriter) failAll(batch []*MutationIntent, err error) {
	for _, intent := range batch {
		if intent.ResultCh != nil {
			select {
			case intent.ResultCh <- err:
			default:
			}
		}
	}
}
