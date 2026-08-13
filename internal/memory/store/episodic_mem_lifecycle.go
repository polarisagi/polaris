package store

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/memory/util"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// EpisodicMem 的生命周期维护：语义巩固、冷标记、高显著性扫描与遗忘回收
// （R7 拆分自 episodic_mem.go）。写入与检索路径见 episodic_mem.go。

// Consolidate 将高频相似事件压缩蒸馏到 SemanticMem。
// 触发条件: EpisodicMem 事件数 >= consolidateThreshold（当前: 20）。
// 算法:
//  1. 按 TaskType(EventType) 聚类
//  2. 同类事件 >= 3 条，且两两 Simhash 距离 <= 8
//  3. 取最新 3 条合并摘要写入 SemanticMem
//  4. 原始事件打 consolidated=true 标记（不删除，保留审计链）
func (em *EpisodicMem) Consolidate(ctx context.Context, semantic *SemanticMem) error {
	var errs []error
	em.mu.RLock()
	events := make([]types.Event, len(em.events))
	copy(events, em.events)
	em.mu.RUnlock()

	if len(events) < 3 {
		return nil
	}

	// 按 EventType 聚类（EventType 是 string defined type，显式转换）
	groups := make(map[string][]types.Event)
	for _, ev := range events {
		groups[string(ev.Type)] = append(groups[string(ev.Type)], ev)
	}

	for evType, evs := range groups {
		if len(evs) < 3 {
			continue
		}
		// 取最新 3 条做 Simhash 相似验证
		recent := evs
		if len(recent) > 3 {
			recent = recent[len(recent)-3:]
		}
		fp0 := util.SimhashOf(string(recent[0].Payload))
		fp1 := util.SimhashOf(string(recent[1].Payload))
		fp2 := util.SimhashOf(string(recent[2].Payload))
		if !util.IsSimilar(fp0, fp1) && !util.IsSimilar(fp1, fp2) {
			continue // 不够相似，跳过合并
		}

		// 构造合并摘要
		summary := ""
		for _, ev := range recent {
			payload := string(ev.Payload)
			if len(payload) > 200 {
				payload = payload[:200]
			}
			summary += payload + " | "
		}
		docID := "consolidated_" + evType + "_" + recent[len(recent)-1].ID
		doc := types.Document{
			ID:         docID,
			Title:      "Consolidated: " + evType,
			SourceType: "episodic",
			SourceURI:  summary, // 摘要存入 SourceURI（Document 无 Content 字段）
		}
		if semantic == nil {
			continue
		}
		// 逐条累积而非首错即返：巩固是把多个事件类型分别蒸馏进语义记忆，
		// 中途 return 会让前几类已巩固、后几类静默丢失，产生偏斜的语义层。
		if err := semantic.StoreDocument(ctx, doc, types.TaintNone); err != nil {
			// 此前是 `_ =`：巩固失败即该批 episodic 事件的语义提炼永久丢失
			// （事件本身随后会被冷标记/归档，不会再被巩固一次），
			// 而 Consolidate 返回 nil 让调用方以为一切正常。
			errs = append(errs, apperr.Wrap(apperr.CodeInternal, "doc "+docID, err))
		}
	}
	if len(errs) > 0 {
		return apperr.Wrap(apperr.CodeInternal,
			"EpisodicMem.Consolidate: partial semantic consolidation failure", errors.Join(errs...))
	}
	return nil
}

// MarkCold 找出 before 时间点之前的 active 事件，并将其冷冻（archived=1）。
// 返回更新的记录数。
func (em *EpisodicMem) MarkCold(ctx context.Context, sessionID string, before time.Time) (int, error) {
	if em.store == nil {
		return 0, nil
	}

	sqlStore, ok := em.store.(protocol.SQLQuerier)
	if !ok {
		return 0, nil
	}

	query := "UPDATE episodic_events SET archived = 1 WHERE session_id = ? AND timestamp < ? AND archived = 0"
	result, err := sqlStore.ExecContext(ctx, query, sessionID, before.Unix())
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "episodic_mem: mark cold failed", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		slog.WarnContext(ctx, "episodic_mem: RowsAffected failed", "error", err)
		return 0, nil
	}

	// 同步清理 KV 中仍未被 OutboxWorker 投影但已过期的事件。
	// 避免 SQL-only UPDATE 遗漏仍在 KV 的新近事件。
	if affected >= 0 { // affected >= 0 保证 SQL 步骤已执行
		em.mu.Lock()
		filtered := em.events[:0]
		var toDelete []string
		for _, ev := range em.events {
			if !ev.CreatedAt.IsZero() && ev.CreatedAt.Before(before) && ev.TaskID == sessionID {
				toDelete = append(toDelete, ev.ID)
			} else {
				filtered = append(filtered, ev)
			}
		}
		em.events = filtered
		em.mu.Unlock()

		for _, id := range toDelete {
			// best-effort：内存索引已移除，KV 删除失败只会留下一条**孤儿记录**
			// ——再也不会被读到（不在索引里），但永久占用存储。不阻断流程，
			// 但必须可观测：静默的存储泄漏会在几个月后以"磁盘满"的形式出现，
			// 届时没有任何线索指向这里。
			if err := em.store.Delete(ctx, []byte("episodic:"+id)); err != nil {
				slog.WarnContext(ctx, "episodic_mem: KV delete failed, orphan record leaked",
					"id", id, "err", err)
			}
		}
	}

	if affected > 0 {
		insertLog := `INSERT INTO episodic_events_change_log
			(session_id, changed_at, change_type, affected_count)
			VALUES (?, ?, 'mark_cold', ?)`
		if _, err := sqlStore.ExecContext(ctx, insertLog, sessionID, time.Now().Unix(), affected); err != nil {
			return 0, apperr.Wrap(apperr.CodeInternal, "episodic_mem: write change_log failed", err)
		}
	}

	return int(affected), nil
}

// ScanHighSalience 扫描 episodic_events 物化表中的高显著性事件（archived=0 且 salience >= 阈值）。
// sinceID 为高水位标记，只返回 id > sinceID 的事件，按 id 升序、limit 截断。
// 供后台维护 Agent（swarm.MemoryAgent）生成耳语提示，取代其对本包的直接 SQL 访问。
// store 未实现 protocol.SQLQuerier（无 SQLite 后端）时静默返回空结果。
func (em *EpisodicMem) ScanHighSalience(ctx context.Context, sinceID int64, minSalience float64, limit int) ([]types.SalienceEvent, error) {
	if em.store == nil {
		return nil, nil
	}
	sqlStore, ok := em.store.(protocol.SQLQuerier)
	if !ok {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}

	rows, err := sqlStore.QueryContext(ctx, `
		SELECT id, session_id, content, salience, COALESCE(occurred_at, timestamp)
		FROM episodic_events
		WHERE archived = 0 AND salience >= ? AND id > ?
		ORDER BY id ASC LIMIT ?
	`, minSalience, sinceID, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "episodic_mem: scan high salience failed", err)
	}
	defer rows.Close()

	var results []types.SalienceEvent //nolint:prealloc
	for rows.Next() {
		var e types.SalienceEvent
		if scanErr := rows.Scan(&e.ID, &e.SessionID, &e.Content, &e.Salience, &e.OccurredAt); scanErr != nil {
			continue
		}
		results = append(results, e)
	}
	return results, nil
}

func (em *EpisodicMem) loadEventsFromStore(ctx context.Context) ([]types.Event, error) {
	iter, err := em.store.Scan(ctx, []byte("episodic:"))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "EpisodicMem.loadEventsFromStore", err)
	}

	var loaded []types.Event
	for iter.Next() {
		var ev types.Event
		if jsonErr := json.Unmarshal(iter.Value(), &ev); jsonErr == nil {
			loaded = append(loaded, ev)
		}
	}
	iter.Close()

	em.mu.Lock()
	defer em.mu.Unlock()
	if len(em.events) == 0 { // double check
		em.events = append(em.events, loaded...)
		if em.maxEvents > 0 && len(em.events) > em.maxEvents {
			em.events = em.events[len(em.events)-em.maxEvents:]
		}
	}
	events := make([]types.Event, len(em.events))
	copy(events, em.events)
	return events, nil
}
