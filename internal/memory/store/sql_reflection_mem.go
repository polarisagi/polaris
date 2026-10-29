package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// SQLReflectionMem — reflection_memory 表实现（替代 KV 前缀存储）
// ============================================================================
// DDL 权威源：internal/protocol/schema/024_reflection_memory.sql
// 写路径：AppendReflection → INSERT INTO reflection_memory（idx_reflect_task_type 索引）
// 读路径：ListReflections → 索引覆盖 SELECT，无全表扫描
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

// ListReflections 按条件查询反思记录，利用 idx_reflect_task_type 索引避免全表扫描。
func (rm *SQLReflectionMem) ListReflections( //nolint:gocyclo
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
		// L3：meta_json 反序列化失败时 e.Meta 保持 nil，下方紧接着会兜底为空 map，
		// 检索侧按"无扩展元数据"处理，不影响 ReflectionEntry 核心字段的正确性。
		if metaStr != "" && metaStr != "{}" {
			if err := json.Unmarshal([]byte(metaStr), &e.Meta); err != nil {
				slog.Warn("memory/sql_reflection_mem: meta_json 反序列化失败，按空 meta 处理", "reflection_id", e.ID, "err", err)
				metrics.RecordMemoryJSONDecodeFailure(ctx, "reflection_memory.meta_json")
			}
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
		concurrent.SafeGo(context.Background(), "sql_reflection_mem.lru_touch", func(_ context.Context) {
			bCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if len(ids) == 0 {
				return
			}
			placeholders := strings.Repeat("?,", len(ids))
			placeholders = strings.TrimSuffix(placeholders, ",")
			args := make([]any, 0, len(ids)+1)
			args = append(args, now)
			for _, id := range ids {
				args = append(args, id)
			}
			// LRU 时间戳写失败不影响本次查询结果，但会让 enforceCapacity 的
			// ORDER BY last_accessed_at 依据陈旧数据淘汰——热条目可能被当成
			// 冷条目删掉。属"降级可接受、静默不可接受"，留一条告警。
			if _, err := rm.db.ExecContext(bCtx,
				fmt.Sprintf("UPDATE reflection_memory SET last_accessed_at = ?, accessed_count = accessed_count + 1 WHERE id IN (%s)", placeholders),
				args...); err != nil {
				slog.Warn("memory/sql_reflection_mem: LRU 时间戳更新失败，淘汰顺序将依据陈旧数据",
					"ids", len(ids), "err", err)
			}
		})
	}

	return results, nil
}

// enforceCapacity 在 append 前检查总量，超出 HT0 上限则 LRU 淘汰一批。
//
// 2026-08-08 补埋点：两处错误此前都是完全静默（`return` / `_, _ =`）。本函数
// 是 reflection_memory 唯一的容量闸门，两条失败路径的后果都是"淘汰没发生但
// 没人知道"——表持续增长直到超出 Tier-0 的 2GB 预算。不向上返回错误是刻意的
// （淘汰属维护动作，失败不该让 Append 一并失败），但必须留痕，否则违反 HE-1
// "禁止能算不上报"。
func (rm *SQLReflectionMem) enforceCapacity(ctx context.Context) {
	var count int
	if err := rm.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM reflection_memory").Scan(&count); err != nil {
		slog.WarnContext(ctx, "memory/sql_reflection_mem: 容量检查失败，本轮跳过淘汰",
			"err", err)
		return
	}
	if count < reflectHT0Limit {
		return
	}
	if _, err := rm.db.ExecContext(ctx, `
		DELETE FROM reflection_memory
		WHERE id IN (
			SELECT id FROM reflection_memory
			ORDER BY last_accessed_at ASC
			LIMIT ?
		)
	`, reflectEvictBatch); err != nil {
		slog.WarnContext(ctx, "memory/sql_reflection_mem: LRU 淘汰失败，表将继续超限增长",
			"count", count, "limit", reflectHT0Limit, "err", err)
	}
}

// 编译期确认 SQLReflectionMem 实现 protocol.ReflectionMemory 接口
var _ protocol.ReflectionMemory = (*SQLReflectionMem)(nil)
