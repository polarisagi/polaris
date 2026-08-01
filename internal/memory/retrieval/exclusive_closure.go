package retrieval

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
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
		// L1（P0 语义）：标记失败意味着旧信念与新信念并存，属事实层数据损坏，
		// 必须向上传播中止本次写入，而不是带着损坏的信念状态继续 UpsertFact。
		if err := w.handleExistingEntity(ctx, existing); err != nil {
			return err
		}
	}

	// Jaccard 近似碰撞检测：仅对 user_preference 类型启用（性能敏感，范围受控）
	if e.Type == "user_preference" {
		if err := w.supersedeSimilarPreferences(ctx, e.Name); err != nil {
			return err
		}
	}

	if err := w.semantic.UpsertFact(ctx, *e, maxTaint); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "ExclusiveWriter.UpsertFactExclusive", err)
	}
	return nil
}

func (w *ExclusiveWriter) handleExistingEntity(ctx context.Context, existing *types.Entity) error {
	if existing.Status != "" && existing.Status != "active" {
		return nil
	}
	return w.supersedeAndCascade(ctx, existing.DBID)
}

// supersedeSimilarPreferences 将与 newName Jaccard > 0.6 的活跃 user_preference 标记 superseded。
//
// GD-14-001 复核修复：此前仅调用 MarkEntitySuperseded，未触发级联失效——与精确碰撞
// 分支（handleExistingEntity）行为不一致，导致依赖"被 Jaccard 判定为同一偏好旧版本"
// 的派生实体（如基于该偏好推导出的下游建议）永远不会进入 pending_review 复核队列，
// 属于同一个 belief revision 语义下的静默遗漏。现改为与精确碰撞分支共用
// supersedeAndCascade，保证两条 superseded 路径的下游一致性。
func (w *ExclusiveWriter) supersedeSimilarPreferences(ctx context.Context, newName string) error {
	// AsOf 传入 0 代表当前时间
	actives, err := w.semantic.ListActiveEntities(ctx, "user_preference", 30, 0)
	if err != nil {
		// 候选列表读取失败与"未发现近似碰撞"在语义上等价（均不改变任何已有信念），
		// 非本次 L1 定级范围（GR-5-002 只点名 supersedeAndCascade 的写入失败），维持原行为。
		return nil
	}
	for _, act := range actives {
		if act.Name == newName {
			continue // 精确碰撞已在调用方处理
		}
		if JaccardSimilarity(act.Name, newName) > 0.6 {
			if err := w.supersedeAndCascade(ctx, act.DBID); err != nil {
				return err
			}
		}
	}
	return nil
}

// supersedeAndCascade 标记实体为 superseded，并（若已注入 CascadeInvalidator）触发级联失效
// 与审计留痕。精确碰撞（handleExistingEntity）与 Jaccard 近似碰撞
// （supersedeSimilarPreferences）共用此逻辑，确保两条 belief revision 路径的下游行为一致。
func (w *ExclusiveWriter) supersedeAndCascade(ctx context.Context, dbID int64) error {
	// L1（P0 语义）：标记失败 → 旧信念与新信念并存，属事实层数据损坏，必须向上传播。
	if err := w.semantic.MarkEntitySuperseded(ctx, dbID, 0); err != nil {
		metrics.GlobalMemorySupersedeFailuresTotal.Add(1)
		return apperr.Wrap(apperr.CodeInternal, "ExclusiveWriter.supersedeAndCascade: mark superseded failed", err)
	}
	if dbID <= 0 {
		return nil
	}
	if w.cascadeInv != nil {
		affected, err := w.cascadeInv.Invalidate(ctx, dbID)
		if err != nil {
			slog.Warn("cascade invalidation failed", "entity_id", dbID, "err", err)
		} else if len(affected) > 0 {
			slog.Info("cascade invalidation triggered", "source", dbID, "affected_count", len(affected))
		}
	}
	if w.db != nil {
		// L4：审计留痕写入失败无补救动作（不影响 superseded 主状态的正确性），
		// 已有 cascade invalidation 的 Warn/Info 日志兜底可观测性。
		if _, err := w.db.ExecContext(ctx,
			`INSERT INTO episodic_events_change_log(session_id, changed_at, change_type, affected_count)
			 VALUES ('belief_revision', ?, 'superseded', 1)`,
			time.Now().UnixMilli()); err != nil {
			slog.Debug("exclusive_closure: belief revision audit log write failed", "entity_id", dbID, "err", err)
		}
	}
	return nil
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
