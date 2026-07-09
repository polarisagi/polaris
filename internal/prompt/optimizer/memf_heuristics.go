package optimizer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ============================================================================
// 成功启发式库 (HeuristicsMemory) + 认知盲区探测器 (BlindZoneDetector)
// （R7 拆分自 memf.go）。谬误记忆池 FallacyMemoryPool 见 memf.go。
// 架构文档: docs/arch/M09-Self-Improvement-Engine.md §2.1（HeuristicsMemory），
// V8-S4 缓解机制（BlindZoneDetector）。
// ============================================================================

// HeuristicsMemory 成功启发式库。
type HeuristicsMemory struct {
	DB protocol.SQLQuerier
}

func NewHeuristicsMemory(DB protocol.SQLQuerier) *HeuristicsMemory {
	return &HeuristicsMemory{DB: DB}
}

// Heuristic 单条启发式规则。
type Heuristic struct {
	ID          string
	Content     string
	TaskType    string
	SuccessRate float64
	UseCount    int
	Keywords    []string
}

// GetRelevant 取 task_type 最相关的 top-5。
func (hm *HeuristicsMemory) GetRelevant(ctx context.Context, taskType string, keywords []string) ([]*Heuristic, error) {
	// 由于降级，这里直接取同 TaskType 的高 success_rate 数据。
	rows, err := hm.DB.QueryContext(ctx, `
		SELECT id, content, task_type, success_rate, use_count, keywords_json
		FROM heuristics_memory
		WHERE task_type = ?
		ORDER BY success_rate DESC, use_count DESC
		LIMIT 5
	`, taskType)

	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "HeuristicsMemory.GetRelevant", err)
	}
	defer rows.Close()

	var heurs []*Heuristic
	for rows.Next() {
		var h Heuristic
		var kwJSON string
		if err := rows.Scan(&h.ID, &h.Content, &h.TaskType, &h.SuccessRate, &h.UseCount, &kwJSON); err != nil {
			slog.Error("swarm: scan heuristics", "err", err)
			continue
		}
		if err := json.Unmarshal([]byte(kwJSON), &h.Keywords); err != nil {
			slog.Warn("memf: failed to unmarshal heuristics keywords_json", "err", err, "id", h.ID)
			h.Keywords = []string{}
		}
		heurs = append(heurs, &h)
	}

	return heurs, nil
}

// Add 将新启发式规则写入 SQLite。
// 若 ID 已存在则更新 success_rate 和 use_count（UPSERT）。
func (hm *HeuristicsMemory) Add(ctx context.Context, h *Heuristic) error {
	kwBytes, _ := json.Marshal(h.Keywords)
	_, err := hm.DB.ExecContext(ctx, `
		INSERT INTO heuristics_memory (id, content, task_type, success_rate, use_count, keywords_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			success_rate = (success_rate * use_count + excluded.success_rate) / (use_count + 1),
			use_count = use_count + 1
	`, h.ID, h.Content, h.TaskType, h.SuccessRate, h.UseCount, string(kwBytes), time.Now().Unix())
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "HeuristicsMemory.Add", err)
	}
	return nil
}

// UpdateSuccessRate 更新启发式规则的成功率（EWMA α=0.1）。
func (hm *HeuristicsMemory) UpdateSuccessRate(ctx context.Context, id string, success bool) error {
	var currentRate float64
	err := hm.DB.QueryRowContext(ctx, "SELECT success_rate FROM heuristics_memory WHERE id = ?", id).Scan(&currentRate)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // 记录不存在，跳过
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "HeuristicsMemory.UpdateSuccessRate", err)
	}
	var observation float64
	if success {
		observation = 1.0
	}
	// EWMA α=0.1
	newRate := 0.9*currentRate + 0.1*observation
	_, err = hm.DB.ExecContext(ctx,
		"UPDATE heuristics_memory SET success_rate = ?, use_count = use_count + 1 WHERE id = ?",
		newRate, id,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "HeuristicsMemory.UpdateSuccessRate", err)
	}
	return nil
}

// ============================================================================
// BlindZoneDetector — 认知盲区探测器（V8-S4 缓解机制）
// 追踪在生产中出现但 MEMF 无任何记录的任务类型。
// 生产出现 ≥5 次且 MEMF 记录 ≤1 → BlindZone 候选，触发强制 System2 + HITL。
// ============================================================================

// BlindZoneDetector 认知盲区探测器。
type BlindZoneDetector struct {
	mu                    sync.RWMutex
	productionOccurrences map[string]int      // taskType → 生产出现次数
	firstSeenAt           map[string]int64    // taskType → 首次出现 Unix 时间戳
	DB                    protocol.SQLQuerier // 只读，用于查询 fallacy_records 表
}

// BlindZoneEntry 盲区条目（用于日志与 OTel 输出）。
type BlindZoneEntry struct {
	TaskType        string
	ProductionCount int
	MemfRecordCount int
	FirstSeenAt     int64
}

// NewBlindZoneDetector 构造探测器。db 用于查询 fallacy_records（010_self_improve.sql 定义）。
func NewBlindZoneDetector(DB protocol.SQLQuerier) *BlindZoneDetector {
	return &BlindZoneDetector{
		productionOccurrences: make(map[string]int),
		firstSeenAt:           make(map[string]int64),
		DB:                    DB,
	}
}

// RecordProduction 记录 taskType 在生产中的一次出现。
// 由 pkg/cognition/kernel/agent_execute.go S_PLAN 阶段开头调用。
func (d *BlindZoneDetector) RecordProduction(taskType string) {
	if taskType == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.productionOccurrences[taskType]++
	if _, ok := d.firstSeenAt[taskType]; !ok {
		d.firstSeenAt[taskType] = time.Now().Unix()
	}
}

// IsBlindZone 判断 taskType 是否为活跃盲区候选。
// 条件：生产出现 ≥5 次 AND MEMF 记录 ≤1（SQL 查询 fallacy_records）。
// 返回 true 时调用方须强制 System2 路由。
func (d *BlindZoneDetector) IsBlindZone(ctx context.Context, taskType string) bool {
	d.mu.RLock()
	count := d.productionOccurrences[taskType]
	d.mu.RUnlock()
	if count < 5 {
		return false // 未达到观测阈值，直接跳过 DB 查询
	}
	var memfCount int
	if d.DB != nil {
		_ = d.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM fallacy_records WHERE task_type = ?`, taskType,
		).Scan(&memfCount)
	}
	return memfCount <= 1
}

// MarkResolved 当 MEMF 首次为该 taskType 写入记录时调用，清除盲区标记。
// 由 FallacyMemoryPool.AddRecord() 写入成功后自动调用。
func (d *BlindZoneDetector) MarkResolved(taskType string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// 重置到阈值以下（保留历史，但退出盲区判定）
	if d.productionOccurrences[taskType] >= 5 {
		d.productionOccurrences[taskType] = 3
	}
}

// ActiveBlindZones 返回当前活跃盲区列表（productionCount≥5 且 memf≤1）。
// 用于 OTel gauge 和运维日志。
func (d *BlindZoneDetector) ActiveBlindZones(ctx context.Context) []BlindZoneEntry {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var result []BlindZoneEntry
	for taskType, count := range d.productionOccurrences {
		if count < 5 {
			continue
		}
		var memfCount int
		if d.DB != nil {
			_ = d.DB.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM fallacy_records WHERE task_type = ?`, taskType,
			).Scan(&memfCount)
		}
		if memfCount <= 1 {
			result = append(result, BlindZoneEntry{
				TaskType:        taskType,
				ProductionCount: count,
				MemfRecordCount: memfCount,
				FirstSeenAt:     d.firstSeenAt[taskType],
			})
		}
	}
	return result
}
