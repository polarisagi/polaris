package retrieval

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ExclusiveWriter 封装了写排他闭合逻辑（Belief Revision）。
// 包含精确碰撞与 Jaccard 近似碰撞（仅 user_preference）检测，
// 并在成功闭合旧事实后异步触发 CascadeInvalidator。
type ExclusiveWriter struct {
	semantic   protocol.SemanticMemory
	cascadeInv *CascadeInvalidator
	db         protocol.SQLQuerier
}

// NewExclusiveWriter 创建排他写入器。
func NewExclusiveWriter(semantic protocol.SemanticMemory, cascadeInv *CascadeInvalidator, db protocol.SQLQuerier) *ExclusiveWriter {
	return &ExclusiveWriter{
		semantic:   semantic,
		cascadeInv: cascadeInv,
		db:         db,
	}
}

// UpsertFactExclusive 写入前进行排他性检查与级联失效，再调用底层 UpsertFact。
func (w *ExclusiveWriter) UpsertFactExclusive(ctx context.Context, e *types.Entity, maxTaint types.TaintLevel) error {
	// 精确碰撞检测：同名同类型已存在 active 实体 → 标记旧版本 superseded
	if existing, err := w.semantic.GetEntity(ctx, e.Type, e.Name); err == nil && existing != nil {
		w.handleExistingEntity(ctx, existing)
	}

	// Jaccard 近似碰撞检测：仅对 user_preference 类型启用（性能敏感，范围受控）
	if e.Type == "user_preference" {
		w.supersedeSimilarPreferences(ctx, e.Name)
	}

	if err := w.semantic.UpsertFact(ctx, *e, maxTaint); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "ExclusiveWriter.UpsertFactExclusive", err)
	}
	return nil
}

func (w *ExclusiveWriter) handleExistingEntity(ctx context.Context, existing *types.Entity) {
	if existing.Status != "" && existing.Status != "active" {
		return
	}
	_ = w.semantic.MarkEntitySuperseded(ctx, existing.DBID, 0)
	if existing.DBID <= 0 {
		return
	}
	if w.cascadeInv != nil {
		affected, err := w.cascadeInv.Invalidate(ctx, existing.DBID)
		if err != nil {
			slog.Warn("cascade invalidation failed", "entity_id", existing.DBID, "err", err)
		} else if len(affected) > 0 {
			slog.Info("cascade invalidation triggered", "source", existing.DBID, "affected_count", len(affected))
		}
	}
	if w.db != nil {
		_, _ = w.db.ExecContext(ctx,
			`INSERT INTO episodic_events_change_log(session_id, changed_at, change_type, affected_count)
			 VALUES ('belief_revision', ?, 'superseded', 1)`,
			time.Now().UnixMilli())
	}
}

// supersedeSimilarPreferences 将与 newName Jaccard > 0.6 的活跃 user_preference 标记 superseded。
func (w *ExclusiveWriter) supersedeSimilarPreferences(ctx context.Context, newName string) {
	// AsOf 传入 0 代表当前时间
	actives, err := w.semantic.ListActiveEntities(ctx, "user_preference", 30, 0)
	if err != nil {
		return
	}
	for _, act := range actives {
		if act.Name == newName {
			continue // 精确碰撞已在调用方处理
		}
		if JaccardSimilarity(act.Name, newName) > 0.6 {
			_ = w.semantic.MarkEntitySuperseded(ctx, act.DBID, 0)
		}
	}
}

// JaccardSimilarity 计算两个字符串的 token 级 Jaccard 相似度 [0,1]。
// 分词: 小写化 + 按空格/下划线/驼峰分割。
func JaccardSimilarity(a, b string) float64 {
	tokA := jaccardTokenize(a)
	tokB := jaccardTokenize(b)
	if len(tokA) == 0 || len(tokB) == 0 {
		return 0
	}
	setA := make(map[string]bool, len(tokA))
	for _, t := range tokA {
		setA[t] = true
	}
	setB := make(map[string]bool, len(tokB))
	for _, t := range tokB {
		setB[t] = true
	}
	intersection := 0
	for t := range setA {
		if setB[t] {
			intersection++
		}
	}
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 1.0
	}
	return float64(intersection) / float64(union)
}

// jaccardTokenize 将字符串分割为小写 token 集合。
func jaccardTokenize(s string) []string {
	s = strings.ToLower(s)
	var tokens []string
	cur := strings.Builder{}
	for _, r := range s {
		if r == ' ' || r == '_' || r == '-' || r == '.' || r == '/' {
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		} else {
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}
