// process_staging.go — 处理 migration staging 中的记忆, 渐进吸收到主线 EventLog。
// 设计意图: staging 中的记忆以低 salience (0.3) 隔离写入，不直接参与 M5 记忆检索。
// process-staging 通过三阶段处理 (去重→salience 重算→topic 提升) 将经过验证的记忆逐步吸收到主线，
// 避免外部迁移的低质量记忆污染 EventLog。仅在用户显式执行 `polaris memory process-staging` 时触发。
// 架构文档: docs/arch/M13-Interface-Scheduler.md §1.1 "外部平台迁移"
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func runProcessStaging() error {
	polarisDB := resolvePolarisDB()

	db, err := sql.Open("sqlite", polarisDB)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "打开 polaris 数据库失败: "+polarisDB, err)
	}
	defer db.Close()

	// 检查 migration_staging 表是否存在——未运行迁移或 staging 表未创建时给出明确指引
	var name string
	err = db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='migration_staging'").Scan(&name)
	if err != nil {
		return apperr.Wrap(apperr.CodeNotFound, "migration_staging 表未找到——请先运行 polaris migrate openclaw", err)
	}

	rows, err := db.Query(`SELECT id, batch_id, total_rows, source_file, topic FROM migration_staging WHERE processed = 0 ORDER BY id`)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "查询 staging 批次失败", err)
	}
	defer rows.Close()

	type batch struct {
		dbID      int
		batchID   string
		totalRows int
		source    string
		topic     string
	}
	var batches []batch
	for rows.Next() {
		var b batch
		if err := rows.Scan(&b.dbID, &b.batchID, &b.totalRows, &b.source, &b.topic); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "扫描 batch 记录失败", err)
		}
		batches = append(batches, b)
	}
	rows.Close()

	if len(batches) == 0 {
		fmt.Println("polaris memory process-staging: no pending batches")
		return nil
	}

	totalStaging := 0
	totalPromoted := 0

	for _, b := range batches {
		fmt.Printf("\n━━━ batch %s (%d staging rows) ━━━\n", b.batchID, b.totalRows)

		db.Exec("UPDATE migration_staging SET processed = 1 WHERE id = ?", b.dbID) //nolint:errcheck

		stagingEvents, err := readStagingEvents(db, b.batchID)
		if err != nil {
			fmt.Printf("WARN  read staging events for %s: %v\n", b.batchID, err)
			db.Exec("UPDATE migration_staging SET processed = 0 WHERE id = ?", b.dbID) //nolint:errcheck
			continue
		}

		totalStaging += len(stagingEvents)

		// Phase 1: 内容指纹去重——相同 batch 内高相似度事件视为重复，仅保留首条
		deduped := deduplicateStaging(stagingEvents)
		fmt.Printf("  Phase1 去重: %d → %d\n", len(stagingEvents), len(deduped))

		// Phase 2: 按角色/内容长度/摘要标记重新计算 salience，低价值记忆保持低权重
		for i := range deduped {
			deduped[i].Salience = recalcSalience(deduped[i])
		}

		// Phase 3: 将经过验证的记忆从 staging topic 提升到主线，设置 episodic 层
		promoted, err := promoteToMain(db, deduped)
		if err != nil {
			fmt.Printf("WARN  promote: %v\n", err)
			db.Exec("UPDATE migration_staging SET processed = 0 WHERE id = ?", b.dbID) //nolint:errcheck
			continue
		}
		totalPromoted += promoted

		db.Exec("UPDATE migration_staging SET processed = 2, processed_at = ? WHERE id = ?", //nolint:errcheck
			time.Now().UnixMilli(), b.dbID)

		fmt.Printf("  DONE  promoted %d to main events (topic promoted, salience recalculated)\n", promoted)
	}

	fmt.Printf("\nDONE  process-staging complete: %d staging → %d promoted to main\n",
		totalStaging, totalPromoted)
	return nil
}

// ─── Staging 事件读取 ──────────────────────────────────────────────────────────────────

type stagingEvent struct {
	Offset    int64
	ID        string
	Topic     string
	Payload   string
	EventType string
	Salience  float64
	Occurred  int64
}

func readStagingEvents(db *sql.DB, batchID string) ([]stagingEvent, error) {
	actorPrefix := fmt.Sprintf("migration:openclaw/%s", batchID)
	rows, err := db.Query(
		`SELECT offset, id, topic, CAST(payload AS TEXT), type, salience, occurred_at
		 FROM events WHERE actor = ? ORDER BY offset`,
		actorPrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []stagingEvent
	for rows.Next() {
		var e stagingEvent
		if err := rows.Scan(&e.Offset, &e.ID, &e.Topic, &e.Payload, &e.EventType, &e.Salience, &e.Occurred); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// ─── Phase 1: 去重 ────────────────────────────────────────────────────────────────────

// deduplicateStaging 基于 content 前 80 字符指纹去重。
// staging 中可能存在跨会话的重复模板消息或多轮交互中的重复 prompt，
// 去重后减少主线 EventLog 的噪音。
func deduplicateStaging(events []stagingEvent) []stagingEvent {
	if len(events) <= 1 {
		return events
	}

	var result []stagingEvent //nolint:prealloc
	seen := make(map[string]bool)

	for _, e := range events {
		var payload map[string]any
		if err := json.Unmarshal([]byte(e.Payload), &payload); err != nil {
			result = append(result, e)
			continue
		}
		content, _ := payload["content"].(string)
		if content == "" {
			result = append(result, e)
			continue
		}

		key := strings.ToLower(content)
		if len(key) > 80 {
			key = key[:80]
		}

		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, e)
	}
	return result
}

// ─── Phase 2: Salience 重算 ───────────────────────────────────────────────────────────

// recalcSalience 按启发式规则重新计算显著性。
// staging 默认 salience=0.3 (低优先级)。重新计算基于:
//
//	内容长度 (信息量)、角色 (assistant 回复更有价值)、摘要标记 (MIGRATION SUMMARY 加权)。
//
// 范围限制在 [0.1, 0.8]，避免过度加权或归零。
func recalcSalience(e stagingEvent) float64 {
	var payload map[string]any
	if err := json.Unmarshal([]byte(e.Payload), &payload); err != nil {
		return e.Salience
	}

	base := 0.35

	content, _ := payload["content"].(string)

	if len(content) > 80 {
		base += 0.1
	}
	if len(content) > 200 {
		base += 0.05
	}
	if len(content) < 30 {
		base -= 0.1
	}

	role, _ := payload["role"].(string)
	switch strings.ToLower(role) {
	case "assistant", "ai", "agent":
		base += 0.15
	case "user", "human":
		base += 0.10
	case "system":
		base -= 0.05
	}

	if strings.HasPrefix(content, "[MIGRATION SUMMARY]") {
		base += 0.15
	}

	if title, ok := payload["title"].(string); ok && title != "" {
		base += 0.05
	}

	if base < 0.1 {
		base = 0.1
	}
	if base > 0.8 {
		base = 0.8
	}
	return base
}

// ─── Phase 3: 提升到主线 ──────────────────────────────────────────────────────────────

// promoteToMain 将 staging 中的事件 topic 从 memory.openclaw.staging 更新为 memory.openclaw，
// 同时写入重新计算的 salience 并标记为 episodic 层。
// 仅更新 topic 精确匹配 staging 的事件——防止误改写已提升的事件。
func promoteToMain(db *sql.DB, events []stagingEvent) (int, error) {
	updateStmt, err := db.Prepare(`
		UPDATE events
		SET topic = 'memory.openclaw', salience = ?, memory_layer = 'episodic'
		WHERE id = ? AND topic = 'memory.openclaw.staging'`)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "准备 promote UPDATE 语句失败", err)
	}
	defer updateStmt.Close()

	promoted := 0
	for _, e := range events {
		res, err := updateStmt.Exec(e.Salience, e.ID)
		if err != nil {
			fmt.Printf("  WARN  promote event %s: %v\n", e.ID, err)
			continue
		}
		n, _ := res.RowsAffected()
		promoted += int(n)
	}
	return promoted, nil
}
