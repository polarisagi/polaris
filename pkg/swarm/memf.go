package swarm

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/polarisagi/polaris/pkg/cognition" //nolint:staticcheck
)

// 谬误记忆池 (MEMF) + 成功启发式库 (HeuristicsMemory)。
// 架构文档: docs/arch/M09-Self-Improvement-Engine.md §2.1

// FallacyMemoryPool 失败轨迹向量化打标池。
// MCTS/Best-of-N 剪枝前做相似度过滤。
// MVP 降级：由于没有向量库，我们使用 SQLite 的纯关系型存储，
// 并以 task_type 结合 keyword (以 json 数组形式存储) 做简单近似。
type FallacyMemoryPool struct {
	db         *sql.DB
	calibrator *DynamicDifficultyCalibrator
	mu         sync.Mutex
}

func NewFallacyMemoryPool(db *sql.DB) *FallacyMemoryPool {
	return &FallacyMemoryPool{
		db:         db,
		calibrator: &DynamicDifficultyCalibrator{adjustStep: 0.05, targetSuccessRate: 0.6},
	}
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
func (m *FallacyMemoryPool) AddRecord(record *FallacyRecord) error {
	if record.FailureType == "safety_violation" {
		return nil
	}

	kwBytes, _ := json.Marshal(record.Keywords)

	_, err := m.db.Exec(`
		INSERT INTO fallacy_records (id, task_type, failure_type, keywords_json, reflection, occurrence_count, node_quality_score, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET 
			occurrence_count = occurrence_count + 1,
			node_quality_score = node_quality_score + 0.1
	`, record.ID, record.TaskType, record.FailureType, string(kwBytes), record.Reflection, record.OccurrenceCount, record.NodeQualityScore, record.CreatedAt)

	return err
}

// FeedbackCalibrate 反馈校准。
func (m *FallacyMemoryPool) FeedbackCalibrate(recordID string, success bool) error {
	m.mu.Lock()
	m.calibrator.history = append(m.calibrator.history, DifficultySample{
		TaskType: "fallback", // MVP: using fallback task type for global calibration
		Success:  success,
	})
	m.calibrator.Calibrate()
	// Use the midpoint of currentLow and currentHigh as the representative SurpriseIndex threshold
	threshold := (m.calibrator.currentLow + m.calibrator.currentHigh) / 2
	if threshold == 0 {
		threshold = 0.45 // default midpoint of [0.3, 0.6]
	}
	m.mu.Unlock()

	// 移除硬编码 0.5，使用动态调整后的 SurpriseIndex
	spm := &cognition.SynapticPlasticityManager{}
	spm.FeedbackCalibrate([]string{recordID}, nil, make(map[string]float64), threshold)

	var delta float64
	if success {
		delta = 0.1
	} else {
		delta = -0.05
	}
	_, err := m.db.Exec(`UPDATE fallacy_records SET node_quality_score = node_quality_score + ? WHERE id = ?`, delta, recordID)
	return err
}

// PruneCandidates 返回可剪枝的失败记录。
// 条件: NQS>0.7 + 创建>30天.
func (m *FallacyMemoryPool) PruneCandidates(now int64) ([]*FallacyRecord, error) {
	rows, err := m.db.Query(`
		SELECT id, task_type, failure_type, keywords_json, reflection, occurrence_count, node_quality_score, created_at 
		FROM fallacy_records 
		WHERE node_quality_score > 0.7 AND (? - created_at) > ?
	`, now, int64(30*86400))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []*FallacyRecord
	for rows.Next() {
		var r FallacyRecord
		var kwJSON string
		if err := rows.Scan(&r.ID, &r.TaskType, &r.FailureType, &kwJSON, &r.Reflection, &r.OccurrenceCount, &r.NodeQualityScore, &r.CreatedAt); err != nil {
			continue
		}
		json.Unmarshal([]byte(kwJSON), &r.Keywords) //nolint:errcheck
		candidates = append(candidates, &r)
	}
	return candidates, nil
}

// DeleteRecord 删除记录。
func (m *FallacyMemoryPool) DeleteRecord(recordID string) error {
	_, err := m.db.Exec("DELETE FROM fallacy_records WHERE id = ?", recordID)
	return err
}

// HeuristicsMemory 成功启发式库。
type HeuristicsMemory struct {
	db *sql.DB
}

func NewHeuristicsMemory(db *sql.DB) *HeuristicsMemory {
	return &HeuristicsMemory{db: db}
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
func (hm *HeuristicsMemory) GetRelevant(taskType string, keywords []string) ([]*Heuristic, error) {
	// 由于降级，这里直接取同 TaskType 的高 success_rate 数据。
	rows, err := hm.db.Query(`
		SELECT id, content, task_type, success_rate, use_count, keywords_json 
		FROM heuristics_memory 
		WHERE task_type = ? 
		ORDER BY success_rate DESC, use_count DESC 
		LIMIT 5
	`, taskType)

	if err != nil {
		return nil, err
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
		json.Unmarshal([]byte(kwJSON), &h.Keywords) //nolint:errcheck
		heurs = append(heurs, &h)
	}

	return heurs, nil
}
