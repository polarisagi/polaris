package optimizer

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
)

// 谬误记忆池 (MEMF) + 成功启发式库 (HeuristicsMemory)。
// 架构文档: docs/arch/M09-Self-Improvement-Engine.md §2.1

// FallacyMemoryPool 失败轨迹向量化打标池。
// MCTS/Best-of-N 剪枝前做相似度过滤。
// MVP 降级：由于没有向量库，我们使用 SQLite 的纯关系型存储，
// 并以 task_type 结合 keyword (以 json 数组形式存储) 做简单近似。
type FallacyMemoryPool struct {
	DB         protocol.SQLQuerier
	Calibrator *DynamicDifficultyCalibrator
	mu         sync.Mutex
	blindZone  *BlindZoneDetector // 可选；写入成功后通知 BlindZoneDetector 解除盲区
}

func NewFallacyMemoryPool(DB protocol.SQLQuerier) *FallacyMemoryPool {
	return &FallacyMemoryPool{
		DB:         DB,
		Calibrator: &DynamicDifficultyCalibrator{AdjustStep: 0.05, TargetSuccessRate: 0.6},
	}
}

// InjectBlindZoneDetector 注入盲区探测器（可选，nil 时跳过通知）。
func (m *FallacyMemoryPool) InjectBlindZoneDetector(d *BlindZoneDetector) {
	m.blindZone = d
}

// FallacyRecord 单条失败记录。
type FallacyRecord struct {
	ID               string
	TaskType         string
	FailureType      string
	Keywords         []string // 降级版 Embedding 替代
	Reflection       string
	OccurrenceCount  int
	NodeQualityScore float64 // >0.7 强制剪枝, <0.3 过时
	CreatedAt        int64
}

// AddRecord 添加新失败记录。
// [安全防线]: 显式拒绝 FailureType == "safety_violation" 的记录进入 MEMF。
func (m *FallacyMemoryPool) AddRecord(ctx context.Context, record *FallacyRecord) error {
	if record.FailureType == "safety_violation" {
		return nil
	}

	kwBytes, _ := json.Marshal(record.Keywords)

	_, err := m.DB.ExecContext(ctx, `
		INSERT INTO fallacy_records (id, task_type, failure_type, keywords_json, reflection, occurrence_count, node_quality_score, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET 
			occurrence_count = occurrence_count + 1,
			node_quality_score = node_quality_score + 0.1
	`, record.ID, record.TaskType, record.FailureType, string(kwBytes), record.Reflection, record.OccurrenceCount, record.NodeQualityScore, record.CreatedAt)

	// 写入成功后通知 BlindZoneDetector 解除该 task_type 的盲区标记
	if err == nil && m.blindZone != nil {
		m.blindZone.MarkResolved(record.TaskType)
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "FallacyMemoryPool.AddRecord", err)
	}
	return nil
}

// FeedbackCalibrate 反馈校准。
func (m *FallacyMemoryPool) FeedbackCalibrate(ctx context.Context, recordID string, success bool) error {
	m.mu.Lock()
	m.Calibrator.History = append(m.Calibrator.History, DifficultySample{
		TaskType: "fallback", // MVP: using fallback task type for global calibration
		Success:  success,
	})
	m.Calibrator.Calibrate()
	m.mu.Unlock()

	var delta float64
	if success {
		delta = 0.1
	} else {
		delta = -0.05
	}
	_, err := m.DB.ExecContext(ctx, `UPDATE fallacy_records SET node_quality_score = node_quality_score + ? WHERE id = ?`, delta, recordID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "FallacyMemoryPool.FeedbackCalibrate", err)
	}
	return nil
}

// PruneCandidates 返回可剪枝的失败记录。
// 条件: NQS>0.7 + 创建>30天.
func (m *FallacyMemoryPool) PruneCandidates(ctx context.Context, now int64) ([]*FallacyRecord, error) {
	rows, err := m.DB.QueryContext(ctx, `
		SELECT id, task_type, failure_type, keywords_json, reflection, occurrence_count, node_quality_score, created_at 
		FROM fallacy_records 
		WHERE node_quality_score > 0.7 AND (? - created_at) > ?
	`, now, int64(30*86400))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "FallacyMemoryPool.PruneCandidates", err)
	}
	defer rows.Close()

	var candidates []*FallacyRecord
	for rows.Next() {
		var r FallacyRecord
		var kwJSON string
		if err := rows.Scan(&r.ID, &r.TaskType, &r.FailureType, &kwJSON, &r.Reflection, &r.OccurrenceCount, &r.NodeQualityScore, &r.CreatedAt); err != nil {
			continue
		}
		if err := json.Unmarshal([]byte(kwJSON), &r.Keywords); err != nil {
			slog.Warn("memf: failed to unmarshal keywords_json", "err", err, "id", r.ID)
			r.Keywords = []string{}
		}
		candidates = append(candidates, &r)
	}
	return candidates, nil
}

// DeleteRecord 删除记录。
func (m *FallacyMemoryPool) DeleteRecord(ctx context.Context, recordID string) error {
	_, err := m.DB.ExecContext(ctx, "DELETE FROM fallacy_records WHERE id = ?", recordID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "FallacyMemoryPool.DeleteRecord", err)
	}
	return nil
}

// HeuristicsMemory（成功启发式库）与 BlindZoneDetector（认知盲区探测器）
// 见 memf_heuristics.go（R7 拆分）。

// ExtractTaskType 从任务目标字符串提取规范化任务类型键。
// 取前 3 个非空词的小写形式作为分组 key。
// 示例: "Write a Python function to sort..." → "write_a_python"
// MVP 降级方案：若 StateContext 未来新增 TaskType 字段，直接使用该字段替代。
func ExtractTaskType(goal string) string {
	words := strings.Fields(strings.ToLower(goal))
	if len(words) == 0 {
		return "unknown"
	}
	if len(words) > 3 {
		words = words[:3]
	}
	return strings.Join(words, "_")
}

// GetMaxQualityScore 查询指定任务类型中最高的 node_quality_score。
// 供外部包（如 pkg/swarm）的 SurpriseCalculator 计算 MEMF 惊异贡献时调用。
// DB 为 nil 时返回 0。
func (m *FallacyMemoryPool) GetMaxQualityScore(ctx context.Context, taskType string) float64 {
	if m.DB == nil {
		return 0
	}
	var maxQuality float64
	err := m.DB.QueryRowContext(ctx, `
		SELECT MAX(node_quality_score)
		FROM fallacy_records
		WHERE task_type = ?
	`, taskType).Scan(&maxQuality)
	if err != nil {
		return 0
	}
	return maxQuality
}

type DynamicDifficultyCalibrator struct {
	History           []DifficultySample
	TargetSuccessRate float64 // 0.6
	AdjustStep        float64 // 0.05
	CurrentLow        float64 // SurpriseIndex 下限
	CurrentHigh       float64 // SurpriseIndex 上限
}

// DifficultySample 难度样本。
type DifficultySample struct {
	TaskType      string
	SurpriseIndex float64
	Success       bool
}

// Calibrate 动态调整难度阈值。
// lastN(50); len<20 → static [0.3, 0.6]
// successRate < 0.5 → low-=0.05, high-=0.05 (floor 0.1)
// successRate > 0.7 → low+=0.05, high+=0.05 (cap 0.85)
func (ddc *DynamicDifficultyCalibrator) Calibrate() {
	if len(ddc.History) < 20 {
		ddc.CurrentLow = 0.3
		ddc.CurrentHigh = 0.6
		return
	}

	// 取最近 50 条：历史不足 50 时从头取全部，避免 len-50<0 的负索引 runtime panic
	// （已知触发区间：20 ≤ len < 50，因上方 len<20 已 return）。
	// 分母使用窗口实际长度，而非原来的 max(50, len) —— 后者在 len>50 时以总长除以仅 50 条的计数，
	// 导致成功率被低估，误触发难度下调。
	start := 0
	if len(ddc.History) > 50 {
		start = len(ddc.History) - 50
	}
	window := ddc.History[start:]
	var successes int
	for _, s := range window {
		if s.Success {
			successes++
		}
	}
	rate := float64(successes) / float64(len(window))

	if rate < 0.5 {
		ddc.CurrentLow = maxF(0.1, ddc.CurrentLow-ddc.AdjustStep)
		ddc.CurrentHigh = maxF(0.1, ddc.CurrentHigh-ddc.AdjustStep)
	} else if rate > 0.7 {
		ddc.CurrentLow = minF(0.85, ddc.CurrentLow+ddc.AdjustStep)
		ddc.CurrentHigh = minF(0.85, ddc.CurrentHigh+ddc.AdjustStep)
	}
}

// HeuristicsMemory.Add / UpdateSuccessRate 见 memf_heuristics.go（R7 拆分）。
