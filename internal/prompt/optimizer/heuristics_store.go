package optimizer

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// HeuristicsStore heuristics_memory + fallacy_records 表的读写层。
// DDL 权威源：internal/protocol/schema/010_self_improve.sql
// 写路径遵循 XR-04：同步写，错误不阻断调用方（由 AddAvoidRule/RecordHeuristic 决定处理策略）。
type HeuristicsStore struct {
	db protocol.SQLQuerier
}

// NewHeuristicsStore 创建 HeuristicsStore，db 必须非 nil。
func NewHeuristicsStore(db protocol.SQLQuerier) *HeuristicsStore {
	return &HeuristicsStore{db: db}
}

// ListHeuristics 启动时从 heuristics_memory 恢复策略到内存 map。
func (hs *HeuristicsStore) ListHeuristics(ctx context.Context) (map[string][]*PromptStrategy, error) {
	rows, err := hs.db.QueryContext(ctx, `
		SELECT id, task_type, content, success_rate, use_count
		FROM heuristics_memory
		ORDER BY success_rate DESC
	`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "heuristics_store: load heuristics", err)
	}
	defer rows.Close()

	result := make(map[string][]*PromptStrategy)
	for rows.Next() {
		s := &PromptStrategy{Source: "db"}
		var taskType string
		if err = rows.Scan(&s.ID, &taskType, &s.Template, &s.SuccessRate, &s.UseCount); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "heuristics_store: scan heuristic", err)
		}
		result[taskType] = append(result[taskType], s)
	}
	return result, rows.Err()
}

// ListFallacies 启动时从 fallacy_records 恢复避免规则到内存 map。
func (hs *HeuristicsStore) ListFallacies(ctx context.Context) (map[string]*ErrorPattern, error) {
	rows, err := hs.db.QueryContext(ctx, `
		SELECT id, task_type, reflection, occurrence_count
		FROM fallacy_records
		ORDER BY occurrence_count DESC
	`)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "heuristics_store: load fallacies", err)
	}
	defer rows.Close()

	result := make(map[string]*ErrorPattern)
	for rows.Next() {
		p := &ErrorPattern{}
		if err = rows.Scan(&p.ID, &p.TaskType, &p.AvoidRule, &p.Frequency); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "heuristics_store: scan fallacy", err)
		}
		result[p.ID] = p
	}
	return result, rows.Err()
}

// SaveHeuristic 持久化成功策略（已存在则更新 success_rate 和 use_count）。
func (hs *HeuristicsStore) SaveHeuristic(ctx context.Context, taskType string, s *PromptStrategy) error {
	if s.ID == "" {
		return apperr.New(apperr.CodeInvalidInput, "heuristics_store: strategy ID empty")
	}
	_, err := hs.db.ExecContext(ctx, `
		INSERT INTO heuristics_memory (id, task_type, content, success_rate, use_count, keywords_json, created_at)
		VALUES (?, ?, ?, ?, ?, '[]', ?)
		ON CONFLICT(id) DO UPDATE SET
			success_rate = excluded.success_rate,
			use_count    = excluded.use_count
	`, s.ID, taskType, s.Template, s.SuccessRate, s.UseCount, time.Now().Unix())
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "heuristics_store: save heuristic", err)
	}
	return nil
}

// SaveFallacy 持久化错误规避规则（已存在则累计 occurrence_count）。
func (hs *HeuristicsStore) SaveFallacy(ctx context.Context, p *ErrorPattern) error {
	if p.ID == "" {
		return apperr.New(apperr.CodeInvalidInput, "heuristics_store: pattern ID empty")
	}
	failureType := p.Description
	if failureType == "" {
		failureType = "avoid_rule"
	}
	_, err := hs.db.ExecContext(ctx, `
		INSERT INTO fallacy_records (id, task_type, failure_type, keywords_json, reflection, occurrence_count, node_quality_score, created_at)
		VALUES (?, ?, ?, '[]', ?, 1, 1.0, ?)
		ON CONFLICT(id) DO UPDATE SET
			occurrence_count = occurrence_count + 1
	`, p.ID, p.TaskType, failureType, p.AvoidRule, time.Now().Unix())
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "heuristics_store: save fallacy", err)
	}
	return nil
}
