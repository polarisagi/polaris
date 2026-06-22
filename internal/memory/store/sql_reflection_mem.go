package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// SQLReflectionMem — reflection_memory 表实现（替代 KV 前缀存储）
// ============================================================================
// DDL 权威源：internal/protocol/schema/024_reflection_memory.sql
// 写路径：AppendReflection → INSERT INTO reflection_memory（idx_reflect_task_type 索引）
// 读路径：QueryReflections → 索引覆盖 SELECT，无全表扫描
// 容量约束：HT0 上限 5000 条，LRU 淘汰最久未访问 100 条/批
// 迁移兼容：旧 KV 前缀 "reflection:{id}" 数据保留在 KV 层，等待 GC 自动清理

const reflectHT0Limit = 5000
const reflectEvictBatch = 100

// SQLReflectionMem 元认知反思层 SQL 实现，全量持久化到 reflection_memory 表。
type SQLReflectionMem struct {
	db protocol.SQLQuerier
}

// NewSQLReflectionMem 创建 SQL 实现，db 必须非 nil。
func NewSQLReflectionMem(db protocol.SQLQuerier) *SQLReflectionMem {
	return &SQLReflectionMem{db: db}
}

// AppendReflection 写入一条反思记录。task_type / salience 等结构字段从 Meta 中提取存入专用列。
func (rm *SQLReflectionMem) AppendReflection(ctx context.Context, entry types.ReflectionEntry) error {
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	if entry.Meta == nil {
		entry.Meta = make(map[string]any)
	}

	taskType, _ := entry.Meta["task_type"].(string)
	reflectionType, _ := entry.Meta["reflection_type"].(string)
	content, _ := entry.Meta["content"].(string)
	if content == "" {
		content = entry.Decision // Decision 作为 content 主体的向下兼容
	}
	salience := 0.8
	if s, ok := entry.Meta["salience"].(float64); ok {
		salience = s
	}
	evidenceIDs := "[]"
	if ids, ok := entry.Meta["evidence_event_ids"].([]string); ok && len(ids) > 0 {
		if b, err := json.Marshal(ids); err == nil {
			evidenceIDs = string(b)
		}
	}
	metaJSON, err := json.Marshal(entry.Meta)
	if err != nil {
		metaJSON = []byte("{}")
	}

	rm.enforceCapacity(ctx)

	_, err = rm.db.ExecContext(ctx, `
		INSERT INTO reflection_memory
			(id, session_id, agent_id, task_type, reflection_type, content,
			 fail_reason, strategy, decision, salience,
			 last_accessed_at, evidence_ids_json, meta_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING
	`, entry.ID, entry.SessionID, entry.AgentID,
		taskType, reflectionType, content,
		entry.FailReason, entry.Strategy, entry.Decision,
		salience, time.Now().Unix(),
		evidenceIDs, string(metaJSON), entry.CreatedAt.Unix())
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "sql_reflection_mem: append", err)
	}
	return nil
}

// QueryReflections 按条件查询反思记录，利用 idx_reflect_task_type 索引避免全表扫描。
func (rm *SQLReflectionMem) QueryReflections( //nolint:gocyclo
	ctx context.Context, q types.ReflectionQuery) ([]types.ReflectionEntry, error) {
	var conds []string
	var args []any

	if q.SessionID != "" {
		conds = append(conds, "session_id = ?")
		args = append(args, q.SessionID)
	}
	if q.AgentID != "" {
		conds = append(conds, "agent_id = ?")
		args = append(args, q.AgentID)
	}
	if q.TaskType != "" {
		conds = append(conds, "task_type = ?")
		args = append(args, q.TaskType)
	}
	if q.Topic != "" {
		conds = append(conds, "(decision LIKE ? OR strategy LIKE ?)")
		like := "%" + q.Topic + "%"
		args = append(args, like, like)
	}

	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	k := q.K
	if k <= 0 {
		k = 100
	}

	stmt := fmt.Sprintf(`
		SELECT id, session_id, agent_id, fail_reason, strategy, decision, meta_json, created_at
		FROM reflection_memory
		%s
		ORDER BY created_at DESC
		LIMIT %d
	`, where, k)

	rows, err := rm.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sql_reflection_mem: query", err)
	}
	defer rows.Close()

	var results []types.ReflectionEntry
	var ids []string
	for rows.Next() {
		var e types.ReflectionEntry
		var metaStr string
		var createdAt int64
		if err = rows.Scan(&e.ID, &e.SessionID, &e.AgentID,
			&e.FailReason, &e.Strategy, &e.Decision,
			&metaStr, &createdAt); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "sql_reflection_mem: scan", err)
		}
		e.CreatedAt = time.Unix(createdAt, 0)
		if metaStr != "" && metaStr != "{}" {
			_ = json.Unmarshal([]byte(metaStr), &e.Meta)
		}
		if e.Meta == nil {
			e.Meta = make(map[string]any)
		}
		results = append(results, e)
		ids = append(ids, e.ID)
	}
	if err = rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "sql_reflection_mem: rows", err)
	}

	// LRU 时间戳更新：异步不阻塞查询路径
	if len(ids) > 0 {
		now := time.Now().Unix()
		go func() {
			bCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, id := range ids {
				_, _ = rm.db.ExecContext(bCtx,
					"UPDATE reflection_memory SET last_accessed_at = ?, accessed_count = accessed_count + 1 WHERE id = ?",
					now, id)
			}
		}()
	}

	return results, nil
}

// enforceCapacity 在 append 前检查总量，超出 HT0 上限则 LRU 淘汰一批。
func (rm *SQLReflectionMem) enforceCapacity(ctx context.Context) {
	var count int
	if err := rm.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM reflection_memory").Scan(&count); err != nil {
		return
	}
	if count < reflectHT0Limit {
		return
	}
	_, _ = rm.db.ExecContext(ctx, `
		DELETE FROM reflection_memory
		WHERE id IN (
			SELECT id FROM reflection_memory
			ORDER BY last_accessed_at ASC
			LIMIT ?
		)
	`, reflectEvictBatch)
}

// 编译期确认 SQLReflectionMem 实现 protocol.ReflectionMemory 接口
var _ protocol.ReflectionMemory = (*SQLReflectionMem)(nil)
