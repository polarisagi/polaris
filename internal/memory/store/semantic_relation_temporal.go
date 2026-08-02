package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// activeRelationRow 已存在的活跃关系边快照（belief revision 判定用，ADR-0083）。
type activeRelationRow struct {
	DBID          int64
	Weight        float64
	PropertiesRaw string // 原始 JSON 文本，NULL 时为空字符串
	TaintLevel    int
	Confidence    float64
}

// queryActiveRelation 按 (source,target,relation_type) 查唯一活跃边（uq_semantic_rel_active）。
// 不存在返回 (nil, nil)。
func (sm *SemanticMem) queryActiveRelation(ctx context.Context, sourceID, targetID int64, relationType string) (*activeRelationRow, error) {
	db, err := sm.requireDB()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "queryActiveRelation", err)
	}
	row := db.QueryRowContext(ctx,
		`SELECT id, weight, COALESCE(properties, ''), taint_level, confidence
		FROM semantic_relations
		WHERE source_id = ? AND target_id = ? AND relation_type = ? AND status = 'active'`,
		sourceID, targetID, relationType,
	)
	var r activeRelationRow
	err = row.Scan(&r.DBID, &r.Weight, &r.PropertiesRaw, &r.TaintLevel, &r.Confidence)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "queryActiveRelation: scan failed", err)
	}
	return &r, nil
}

// relationSubstantiallyChanged 判定新写入是否构成"实质变化"（ADR-0083 信念修正阈值）：
// 权重变化超过 threshold，或去除 taint_level 记账字段后的 properties 内容不同。
// 仅证据累积/权重微调（未超阈值且 properties 不变）不算实质变化，原地 UPDATE 即可，
// 避免版本链无谓膨胀。
func relationSubstantiallyChanged(old activeRelationRow, newWeight float64, newPropsJSON string, threshold float64) bool {
	delta := newWeight - old.Weight
	if delta < 0 {
		delta = -delta
	}
	if delta > threshold {
		return true
	}
	return normalizeRelationProps(old.PropertiesRaw) != normalizeRelationProps(newPropsJSON)
}

// normalizeRelationProps 去除 taint_level 记账键后重新序列化，避免因该键的
// only-up 变化被误判为"实质变化"（污点上调不代表事实本身发生了变化）。
func normalizeRelationProps(raw string) string {
	if raw == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return raw // 非法 JSON：原样比较，保守起见仍可能触发版本升级
	}
	delete(m, "taint_level")
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return string(b)
}

// relationWeightDeltaThreshold 读取 SSoT 阈值（state.yaml m5_memory.
// relation_weight_delta_threshold，ADR-0083）。
func relationWeightDeltaThreshold() float64 {
	return config.DefaultThresholds().M5Memory.RelationWeightDeltaThreshold
}

// supersedeActiveRelation 将旧边置为 superseded，记录取代它的新边 DBID（ADR-0083）。
// newDBID<=0 时写 NULL——用于"先腾出 uq_semantic_rel_active 唯一槽位，新行插入
// 后再回填 superseded_by"的两阶段信念修正流程（新行此时尚未插入，ID 未知）。
func (sm *SemanticMem) supersedeActiveRelation(ctx context.Context, oldDBID, newDBID, now int64) error {
	db, err := sm.requireDB()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "supersedeActiveRelation", err)
	}
	var supersededBy any
	if newDBID > 0 {
		supersededBy = newDBID
	}
	_, err = db.ExecContext(ctx,
		`UPDATE semantic_relations SET status = 'superseded', valid_until = ?, superseded_by = ?, updated_at = ?
		WHERE id = ? AND status = 'active'`,
		now, supersededBy, now, oldDBID,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "supersedeActiveRelation: update failed", err)
	}
	return nil
}

// relationWriteParams 是 UpsertRelation 三个写路径分支（新建/原地更新/信念修正）
// 共用的入参集合，下沉到本文件以保持 UpsertRelation 本身只负责编排（R7 + gocyclo）。
type relationWriteParams struct {
	FromDBID, ToDBID int64
	RelationType     string
	Weight           float64
	Confidence       float64
	TaintLevel       int
	SourceEventID    int64
	NullProps        any // 已 json.Marshal 或 nil，直接绑定 SQL 参数
	Now              int64
}

// insertNewActiveRelation 三元组此前无活跃边：直接插入一条新的活跃边。
func (sm *SemanticMem) insertNewActiveRelation(ctx context.Context, p relationWriteParams) error {
	db, err := sm.requireDB()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "insertNewActiveRelation", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO semantic_relations
		    (source_id, target_id, relation_type, weight, properties,
		     created_at, source_event_id, updated_at, confidence, taint_level, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active')`,
		p.FromDBID, p.ToDBID, p.RelationType, p.Weight, p.NullProps,
		p.Now, nullableInt64(p.SourceEventID), p.Now, p.Confidence, p.TaintLevel,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "insertNewActiveRelation: insert failed", err)
	}
	return nil
}

// updateRelationInPlace 未超"实质变化"阈值：原地 UPDATE，不产生新版本
// （权重取 MAX 只升不降，污点/置信度由调用方预先合并为 mergedTaint/mergedConfidence
// 并塞进 p.TaintLevel/p.Confidence）。
func (sm *SemanticMem) updateRelationInPlace(ctx context.Context, existing activeRelationRow, p relationWriteParams) error {
	db, err := sm.requireDB()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "updateRelationInPlace", err)
	}
	mergedTaint := mergeTaintInt(existing.TaintLevel, p.TaintLevel)
	mergedConfidence := mergeMaxFloat(existing.Confidence, p.Confidence)
	_, err = db.ExecContext(ctx, `
		UPDATE semantic_relations SET
		    weight = MAX(weight, ?), updated_at = ?, confidence = ?, taint_level = ?, properties = ?
		WHERE id = ?`,
		p.Weight, p.Now, mergedConfidence, mergedTaint, p.NullProps, existing.DBID,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "updateRelationInPlace: update failed", err)
	}
	return nil
}

// reviseRelation 实质变化：信念修正。三步顺序不可颠倒——uq_semantic_rel_active
// 只允许同一三元组存在一条 status='active' 行，必须先把旧行让出 active 状态
// （superseded_by 暂填 NULL），新行才能插入成功；插入后再回填 superseded_by
// 完成版本链闭合（ADR-0083）。
func (sm *SemanticMem) reviseRelation(ctx context.Context, existing activeRelationRow, p relationWriteParams) error {
	db, err := sm.requireDB()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "reviseRelation", err)
	}
	if err := sm.supersedeActiveRelation(ctx, existing.DBID, 0, p.Now); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "reviseRelation: supersede old version failed", err)
	}

	mergedTaint := mergeTaintInt(existing.TaintLevel, p.TaintLevel)
	mergedConfidence := mergeMaxFloat(existing.Confidence, p.Confidence)
	res, err := db.ExecContext(ctx, `
		INSERT INTO semantic_relations
		    (source_id, target_id, relation_type, weight, properties,
		     created_at, source_event_id, updated_at, confidence, taint_level, status, valid_from)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?)`,
		p.FromDBID, p.ToDBID, p.RelationType, p.Weight, p.NullProps,
		p.Now, nullableInt64(p.SourceEventID), p.Now, mergedConfidence, mergedTaint, p.Now,
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "reviseRelation: insert failed", err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "reviseRelation: new id failed", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE semantic_relations SET superseded_by = ? WHERE id = ?`, newID, existing.DBID,
	); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "reviseRelation: backfill superseded_by failed", err)
	}
	return nil
}

func mergeTaintInt(a, b int) int {
	if b > a {
		return b
	}
	return a
}

func mergeMaxFloat(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
}
