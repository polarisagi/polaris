package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/pb"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// IntentSubmitter abstracts the MutationBus submission to avoid circular dependency.
type IntentSubmitter interface {
	Submit(ctx context.Context, intent *pb.MutationIntent) error
}

// SemanticMemWriter 定义供工具调用修改语义记忆的接口（防循环依赖）。
type SemanticMemWriter interface {
	UpsertFact(ctx context.Context, entity types.Entity) error
	Archive(ctx context.Context, id string, reason string) error
	GetEntity(ctx context.Context, entityType, name string) (*types.Entity, error)
}

// ============================================================================
// SemanticMemory (L2) — 文档/实体存储
// ============================================================================

type SemanticMem struct {
	store protocol.Store
	bus   IntentSubmitter
}

func NewSemanticMem(store protocol.Store, bus IntentSubmitter) *SemanticMem {
	return &SemanticMem{store: store, bus: bus}
}

func (sm *SemanticMem) StoreDocument(ctx context.Context, doc types.Document) error {
	key := []byte("doc:" + doc.ID)
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("SemanticMem.StoreDocument: %w", err)
	}
	return sm.store.Put(ctx, key, data)
}

func (sm *SemanticMem) StoreChunks(ctx context.Context, docID string, chunks []types.Chunk) error {
	for _, ch := range chunks {
		key := []byte("chunk:" + ch.ID)
		data, err := json.Marshal(ch)
		if err != nil {
			return fmt.Errorf("SemanticMem.StoreChunks: %w", err)
		}
		if err := sm.store.Put(ctx, key, data); err != nil {
			return fmt.Errorf("SemanticMem.StoreChunks: %w", err)
		}
	}
	return nil
}

func (sm *SemanticMem) StoreStats() (string, error) {
	if ext, ok := sm.store.(protocol.StoreExtStats); ok {
		return ext.Stats()
	}
	return "{}", nil
}

func (sm *SemanticMem) SetVectorMode(mode int) error {
	if ext, ok := sm.store.(protocol.StoreExtVector); ok {
		return ext.VecSetMode(mode)
	}
	return nil
}

func (sm *SemanticMem) GetDocument(ctx context.Context, id string) (*types.Document, error) {
	data, err := sm.store.Get(ctx, []byte("doc:"+id))
	if err != nil {
		return nil, fmt.Errorf("SemanticMem.GetDocument: %w", err)
	}
	var doc types.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("SemanticMem.GetDocument: %w", err)
	}
	return &doc, nil
}

func (sm *SemanticMem) Archive(ctx context.Context, id string, reason string) error {
	doc, err := sm.GetDocument(ctx, id)
	if err != nil {
		return fmt.Errorf("SemanticMem.Archive: %w", err)
	}
	doc.Archived = true
	return sm.StoreDocument(ctx, *doc)
}

func (sm *SemanticMem) UpsertFact(ctx context.Context, entity types.Entity) error {
	db, err := sm.requireDB()
	if err != nil {
		return fmt.Errorf("SemanticMem.UpsertFact: %w", err)
	}

	now := time.Now().UnixMilli()
	propsJSON, _ := json.Marshal(entity.Properties)
	sourceType := resolveEntitySourceType(entity)
	confidence := entity.Confidence
	if confidence <= 0 {
		confidence = 0.8 // LLM 提取有不确定性，默认不设满分
	}
	status := entity.Status
	if status == "" {
		status = "active"
	}
	validFrom, validUntil := resolveEntityValidWindow(entity)

	_, err = db.ExecContext(ctx, `
		INSERT INTO semantic_entities
		    (entity_type, name, properties, version, created_at, updated_at,
		     source_event_id, status, confidence, source_type, valid_from, valid_until)
		VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(entity_type, name) DO UPDATE SET
		    properties  = excluded.properties,
		    updated_at  = excluded.updated_at,
		    version     = version + 1,
		    confidence  = MAX(confidence, excluded.confidence),
		    source_type = excluded.source_type,
		    valid_from  = COALESCE(excluded.valid_from, valid_from),
		    valid_until = excluded.valid_until,
		    status      = CASE WHEN status IN ('superseded','expired') THEN status ELSE 'active' END`,
		entity.Type, entity.Name, string(propsJSON), now, now,
		nullableInt64(entity.SourceEventID), status, confidence, sourceType,
		nullableInt64(validFrom), nullableInt64(validUntil),
	)
	if err != nil {
		return fmt.Errorf("SemanticMem.UpsertFact: %w", err)
	}
	return nil
}

// resolveEntitySourceType 读取实体的 source_type：结构体字段 → Properties map → 默认值。
// 兼容旧路径（memory_write 工具将 source_type 写入 Properties）。
func resolveEntitySourceType(entity types.Entity) string {
	if entity.SourceType != "" {
		return entity.SourceType
	}
	if st, ok := entity.Properties["source_type"].(string); ok && st != "" {
		return st
	}
	return "llm_extract"
}

// resolveEntityValidWindow 读取 valid_from/valid_until：结构体字段 → Properties map。
// JSON 反序列化后数字类型为 float64，需同时处理 int64 和 float64 两种断言。
func resolveEntityValidWindow(entity types.Entity) (validFrom, validUntil int64) {
	validFrom = entity.ValidFrom
	if validFrom == 0 {
		switch v := entity.Properties["valid_from"].(type) {
		case int64:
			validFrom = v
		case float64:
			validFrom = int64(v)
		}
	}
	validUntil = entity.ValidUntil
	if validUntil == 0 {
		switch v := entity.Properties["valid_until"].(type) {
		case int64:
			validUntil = v
		case float64:
			validUntil = int64(v)
		}
	}
	return
}

// nullableInt64 将零值 int64 转为 nil，对应 SQLite NULL（可空 INTEGER 列）。
// 与 nullableJSON 命名一致，保持辅助函数风格统一。
func nullableInt64(v int64) any {
	if v <= 0 {
		return nil
	}
	return v
}

func (sm *SemanticMem) UpsertRelation(ctx context.Context, rel types.Relation) error {
	// FromDBID/ToDBID 必须由调用方（upsertSemantic）在 UpsertFact 后填充
	if rel.FromDBID <= 0 || rel.ToDBID <= 0 {
		return apperr.New(apperr.CodeInternal,
			"SemanticMem.UpsertRelation: FromDBID/ToDBID not resolved; call GetEntity after UpsertFact to populate")
	}
	db, err := sm.requireDB()
	if err != nil {
		return fmt.Errorf("SemanticMem.UpsertRelation: %w", err)
	}

	now := time.Now().UnixMilli()
	weight := rel.Weight
	if weight <= 0 {
		weight = 1.0
	}
	confidence := rel.Confidence
	if confidence <= 0 {
		confidence = 1.0
	}

	var nullProps any
	if len(rel.Properties) > 0 {
		if b, merr := json.Marshal(rel.Properties); merr == nil {
			nullProps = string(b)
		}
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO semantic_relations
		    (source_id, target_id, relation_type, weight, properties,
		     created_at, source_event_id, updated_at, confidence)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_id, target_id, relation_type) DO UPDATE SET
		    weight     = MAX(weight, excluded.weight),
		    updated_at = excluded.updated_at,
		    confidence = MAX(confidence, excluded.confidence),
		    properties = excluded.properties`,
		rel.FromDBID, rel.ToDBID, rel.RelationType, weight, nullProps,
		now, nullableInt64(rel.SourceEventID), now, confidence,
	)
	if err != nil {
		return fmt.Errorf("SemanticMem.UpsertRelation: %w", err)
	}
	return nil
}

func (sm *SemanticMem) GetEntity(ctx context.Context, entityType, name string) (*types.Entity, error) {
	db, err := sm.requireDB()
	if err != nil {
		return nil, fmt.Errorf("SemanticMem.GetEntity: %w", err)
	}

	const q = `SELECT id, name, entity_type, properties, embedding,
	            COALESCE(source_event_id, 0), version,
	            status, COALESCE(superseded_by, 0),
	            confidence, source_type,
	            COALESCE(valid_from, 0), COALESCE(valid_until, 0)
	            FROM semantic_entities WHERE entity_type = ? AND name = ?`
	row := db.QueryRowContext(ctx, q, entityType, name)

	var ent types.Entity
	var propertiesJSON []byte
	var embeddingBytes []byte

	err = row.Scan(
		&ent.DBID, &ent.Name, &ent.Type, &propertiesJSON, &embeddingBytes,
		&ent.SourceEventID, &ent.Version, &ent.Status, &ent.SupersededBy,
		&ent.Confidence, &ent.SourceType, &ent.ValidFrom, &ent.ValidUntil,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.New(apperr.CodeNotFound, "Entity not found")
		}
		return nil, fmt.Errorf("SemanticMem.GetEntity: %w", err)
	}

	ent.ID = "entity:" + strconv.FormatInt(ent.DBID, 10)
	if len(propertiesJSON) > 0 {
		_ = json.Unmarshal(propertiesJSON, &ent.Properties)
	}
	return &ent, nil
}

// ListActiveEntities 返回指定类型的活跃（status='active'）实体列表，供 Jaccard 矛盾检测使用。
func (sm *SemanticMem) ListActiveEntities(ctx context.Context, entityType string, limit int) ([]types.Entity, error) {
	db, err := sm.requireDB()
	if err != nil {
		return nil, fmt.Errorf("SemanticMem.ListActiveEntities: %w", err)
	}
	if limit <= 0 {
		limit = 50
	}

	const q = `SELECT id, name, entity_type, properties,
	            COALESCE(source_event_id, 0), version,
	            status, COALESCE(superseded_by, 0),
	            confidence, source_type,
	            COALESCE(valid_from, 0), COALESCE(valid_until, 0)
	            FROM semantic_entities WHERE status='active' AND entity_type=?
	            ORDER BY updated_at DESC LIMIT ?`
	rows, err := db.QueryContext(ctx, q, entityType, limit)
	if err != nil {
		return nil, fmt.Errorf("SemanticMem.ListActiveEntities: %w", err)
	}
	defer rows.Close()

	var result []types.Entity
	for rows.Next() {
		var ent types.Entity
		var propertiesJSON []byte
		if err := rows.Scan(
			&ent.DBID, &ent.Name, &ent.Type, &propertiesJSON,
			&ent.SourceEventID, &ent.Version, &ent.Status, &ent.SupersededBy,
			&ent.Confidence, &ent.SourceType, &ent.ValidFrom, &ent.ValidUntil,
		); err != nil {
			continue
		}
		ent.ID = "entity:" + strconv.FormatInt(ent.DBID, 10)
		if len(propertiesJSON) > 0 {
			_ = json.Unmarshal(propertiesJSON, &ent.Properties)
		}
		result = append(result, ent)
	}
	return result, rows.Err()
}

// MarkEntitySuperseded 将旧实体标记为 superseded，并记录取代它的新实体 DBID。
// 仅当当前 status='active' 时生效（防重复超越）。
func (sm *SemanticMem) MarkEntitySuperseded(ctx context.Context, oldDBID int64, newDBID int64) error {
	db, err := sm.requireDB()
	if err != nil {
		return fmt.Errorf("SemanticMem.MarkEntitySuperseded: %w", err)
	}
	var supersededBy any
	if newDBID > 0 {
		supersededBy = newDBID
	}
	_, err = db.ExecContext(ctx,
		`UPDATE semantic_entities SET status='superseded', superseded_by=?, updated_at=?
		 WHERE id=? AND status='active'`,
		supersededBy, time.Now().UnixMilli(), oldDBID)
	if err != nil {
		return fmt.Errorf("SemanticMem.MarkEntitySuperseded: %w", err)
	}
	return nil
}

// UpsertUserProfile 创建或更新用户画像。
func (sm *SemanticMem) UpsertUserProfile(ctx context.Context, profile types.UserProfile) error {
	db, err := sm.requireDB()
	if err != nil {
		return fmt.Errorf("SemanticMem.UpsertUserProfile: %w", err)
	}
	stableFacts, _ := json.Marshal(profile.StableFacts)
	recentActivity, _ := json.Marshal(profile.RecentActivity)
	behavioralPatterns, _ := json.Marshal(profile.BehavioralPatterns)
	now := time.Now().UnixMilli()

	_, err = db.ExecContext(ctx, `
		INSERT INTO user_profile (profile_key, stable_facts, recent_activity, behavioral_patterns,
		                          synthesis_count, last_event_ts, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(profile_key) DO UPDATE SET
		    stable_facts        = excluded.stable_facts,
		    recent_activity     = excluded.recent_activity,
		    behavioral_patterns = excluded.behavioral_patterns,
		    synthesis_count     = excluded.synthesis_count,
		    last_event_ts       = excluded.last_event_ts,
		    updated_at          = excluded.updated_at`,
		profile.ProfileKey,
		nullableJSON(stableFacts), nullableJSON(recentActivity), nullableJSON(behavioralPatterns),
		profile.SynthesisCount, profile.LastEventTS, now, now,
	)
	if err != nil {
		return fmt.Errorf("SemanticMem.UpsertUserProfile: %w", err)
	}
	return nil
}

// GetUserProfile 读取用户画像，不存在返回 nil 和 CodeNotFound 错误。
func (sm *SemanticMem) GetUserProfile(ctx context.Context, profileKey string) (*types.UserProfile, error) {
	db, err := sm.requireDB()
	if err != nil {
		return nil, fmt.Errorf("SemanticMem.GetUserProfile: %w", err)
	}

	const q = `SELECT profile_key, stable_facts, recent_activity, behavioral_patterns,
	            synthesis_count, COALESCE(last_event_ts, 0)
	            FROM user_profile WHERE profile_key=?`
	row := db.QueryRowContext(ctx, q, profileKey)

	var p types.UserProfile
	var stableJSON, recentJSON, behavioralJSON []byte
	err = row.Scan(&p.ProfileKey, &stableJSON, &recentJSON, &behavioralJSON,
		&p.SynthesisCount, &p.LastEventTS)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.New(apperr.CodeNotFound, "user_profile not found")
		}
		return nil, fmt.Errorf("SemanticMem.GetUserProfile: %w", err)
	}
	if len(stableJSON) > 0 {
		_ = json.Unmarshal(stableJSON, &p.StableFacts)
	}
	if len(recentJSON) > 0 {
		_ = json.Unmarshal(recentJSON, &p.RecentActivity)
	}
	if len(behavioralJSON) > 0 {
		_ = json.Unmarshal(behavioralJSON, &p.BehavioralPatterns)
	}
	return &p, nil
}

// ─── 内部辅助 ─────────────────────────────────────────────────────────────────

func (sm *SemanticMem) requireDB() (protocol.SQLQuerier, error) {
	if q, ok := sm.store.(protocol.SQLQuerier); ok && q != nil {
		return q, nil
	}
	if dba, ok := sm.store.(interface{ DB() *sql.DB }); ok {
		if db := dba.DB(); db != nil {
			return db, nil
		}
		return nil, apperr.New(apperr.CodeInternal, "Underlying DB is nil")
	}
	return nil, apperr.New(apperr.CodeInternal, "Store does not implement SQLQuerier")
}

// nullableJSON 将空 JSON 对象/数组转为 nil，防止写入空白 "null" 字节。
func nullableJSON(b []byte) any {
	if len(b) == 0 || string(b) == "null" || string(b) == "{}" || string(b) == "[]" {
		return nil
	}
	return b
}

// ============================================================================
// ProceduralMemory (L3) — 委托 M6 SkillRegistry
// ============================================================================

type ProceduralMem struct {
	skills protocol.SkillRegistry
}

func (pm *ProceduralMem) Skills() protocol.SkillRegistry {
	return pm.skills
}

func NewProceduralMem(skills protocol.SkillRegistry) *ProceduralMem {
	return &ProceduralMem{skills: skills}
}

func (p *ProceduralMem) SetSkills(s protocol.SkillRegistry) { p.skills = s }
