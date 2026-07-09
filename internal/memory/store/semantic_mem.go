package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// ============================================================================
// SemanticMemory (L2) — 文档/实体存储
// ============================================================================
//
// 实体/关系查询扩展（ListActiveEntities/MarkEntitySuperseded/UserProfile/
// SearchEntities）与 ProceduralMemory（L3）见 semantic_mem_query.go（R7 拆分）。

type SemanticMem struct {
	store     protocol.Store
	bus       IntentSubmitter
	cognitive protocol.CognitiveSearcher
}

func NewSemanticMem(store protocol.Store, bus IntentSubmitter) *SemanticMem {
	return &SemanticMem{store: store, bus: bus}
}

func NewSemanticMemWithCognitive(store protocol.Store, bus IntentSubmitter, cognitive protocol.CognitiveSearcher) *SemanticMem {
	return &SemanticMem{store: store, bus: bus, cognitive: cognitive}
}

func (sm *SemanticMem) StoreDocument(ctx context.Context, doc types.Document, taint types.TaintLevel) error {
	doc.Taint = types.PropagateTaint(doc.Taint, taint)
	key := []byte("doc:" + doc.ID)
	data, err := json.Marshal(doc)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SemanticMem.StoreDocument", err)
	}
	return sm.store.Put(ctx, key, data)
}

func (sm *SemanticMem) StoreChunks(ctx context.Context, docID string, chunks []types.Chunk, taint types.TaintLevel) error {
	for _, ch := range chunks {
		key := []byte("chunk:" + ch.ID)
		data, err := json.Marshal(ch)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SemanticMem.StoreChunks", err)
		}
		if err := sm.store.Put(ctx, key, data); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SemanticMem.StoreChunks", err)
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
		return nil, apperr.Wrap(apperr.CodeInternal, "SemanticMem.GetDocument", err)
	}
	var doc types.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SemanticMem.GetDocument", err)
	}
	return &doc, nil
}

func (sm *SemanticMem) Archive(ctx context.Context, id string, reason string) error {
	doc, err := sm.GetDocument(ctx, id)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SemanticMem.Archive", err)
	}
	doc.Archived = true
	err = sm.StoreDocument(ctx, *doc, types.TaintNone)
	return err
}

func (sm *SemanticMem) UpsertFact(ctx context.Context, entity types.Entity, taint types.TaintLevel) error {
	db, err := sm.requireDB()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SemanticMem.UpsertFact", err)
	}

	entity.TaintLevel = types.PropagateTaint(entity.TaintLevel, taint) // only-up
	if entity.Properties == nil {
		entity.Properties = make(map[string]any)
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
		     source_event_id, status, confidence, source_type, valid_from, valid_until, taint_level)
		VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(entity_type, name) DO UPDATE SET
		    properties  = excluded.properties,
		    updated_at  = excluded.updated_at,
		    version     = version + 1,
		    confidence  = MAX(confidence, excluded.confidence),
		    source_type = excluded.source_type,
		    valid_from  = COALESCE(excluded.valid_from, valid_from),
		    valid_until = excluded.valid_until,
		    taint_level = MAX(taint_level, excluded.taint_level),
		    status      = CASE WHEN status IN ('superseded','expired') THEN status ELSE 'active' END`,
		entity.Type, entity.Name, string(propsJSON), now, now,
		nullableInt64(entity.SourceEventID), status, confidence, sourceType,
		nullableInt64(validFrom), nullableInt64(validUntil), int(entity.TaintLevel),
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SemanticMem.UpsertFact", err)
	}

	if sm.cognitive != nil {
		desc, _ := entity.Properties["description"].(string)
		text := entity.Name
		if desc != "" {
			text += " " + desc
		} else {
			if b, err := json.Marshal(entity.Properties); err == nil {
				text += " " + string(b)
			}
		}
		_ = sm.cognitive.FTSIndex("sement_"+entity.Type+"_"+entity.Name, text)
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

func (sm *SemanticMem) UpsertRelation(ctx context.Context, rel types.Relation, taint types.TaintLevel) error {
	// FromDBID/ToDBID 必须由调用方（upsertSemantic）在 UpsertFact 后填充
	if rel.FromDBID <= 0 || rel.ToDBID <= 0 {
		return apperr.New(apperr.CodeInternal,
			"SemanticMem.UpsertRelation: FromDBID/ToDBID not resolved; call GetEntity after UpsertFact to populate")
	}
	db, err := sm.requireDB()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SemanticMem.UpsertRelation", err)
	}

	rel.TaintLevel = types.PropagateTaint(rel.TaintLevel, taint)
	if rel.Properties == nil {
		rel.Properties = make(map[string]any)
	}
	rel.Properties["taint_level"] = int(rel.TaintLevel)

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
		     created_at, source_event_id, updated_at, confidence, taint_level)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_id, target_id, relation_type) DO UPDATE SET
		    weight      = MAX(weight, excluded.weight),
		    updated_at  = excluded.updated_at,
		    confidence  = MAX(confidence, excluded.confidence),
		    taint_level = MAX(taint_level, excluded.taint_level),
		    properties  = excluded.properties`,
		rel.FromDBID, rel.ToDBID, rel.RelationType, weight, nullProps,
		now, nullableInt64(rel.SourceEventID), now, confidence, int(rel.TaintLevel),
	)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SemanticMem.UpsertRelation", err)
	}
	return nil
}

func (sm *SemanticMem) GetEntity(ctx context.Context, entityType, name string) (*types.Entity, error) {
	db, err := sm.requireDB()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SemanticMem.GetEntity", err)
	}

	const q = `SELECT id, name, entity_type, properties, embedding,
	            COALESCE(source_event_id, 0), version,
	            status, COALESCE(superseded_by, 0),
	            confidence, source_type,
	            COALESCE(valid_from, 0), COALESCE(valid_until, 0),
	            COALESCE(taint_level, 0)
	            FROM semantic_entities WHERE entity_type = ? AND name = ?`
	row := db.QueryRowContext(ctx, q, entityType, name)

	var ent types.Entity
	var propertiesJSON []byte
	var embeddingBytes []byte

	err = row.Scan(
		&ent.DBID, &ent.Name, &ent.Type, &propertiesJSON, &embeddingBytes,
		&ent.SourceEventID, &ent.Version, &ent.Status, &ent.SupersededBy,
		&ent.Confidence, &ent.SourceType, &ent.ValidFrom, &ent.ValidUntil,
		(*int)(&ent.TaintLevel),
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.New(apperr.CodeNotFound, "Entity not found")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "SemanticMem.GetEntity", err)
	}

	ent.ID = "entity:" + strconv.FormatInt(ent.DBID, 10)
	if len(propertiesJSON) > 0 {
		_ = json.Unmarshal(propertiesJSON, &ent.Properties)
	}
	if len(embeddingBytes) > 0 {
		ent.Embedding = bytesToFloat32s(embeddingBytes)
	}
	return &ent, nil
}

// GetEntityByID 根据数据库主键加载实体（内部维护用）。
func (sm *SemanticMem) GetEntityByID(ctx context.Context, id int64) (*types.Entity, error) {
	db, err := sm.requireDB()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SemanticMem.GetEntityByID", err)
	}

	const q = `SELECT id, name, entity_type, properties, embedding,
	            COALESCE(source_event_id, 0), version,
	            status, COALESCE(superseded_by, 0),
	            confidence, source_type,
	            COALESCE(valid_from, 0), COALESCE(valid_until, 0),
	            COALESCE(taint_level, 0)
	            FROM semantic_entities WHERE id = ?`
	row := db.QueryRowContext(ctx, q, id)

	var ent types.Entity
	var propertiesJSON []byte
	var embeddingBytes []byte

	err = row.Scan(
		&ent.DBID, &ent.Name, &ent.Type, &propertiesJSON, &embeddingBytes,
		&ent.SourceEventID, &ent.Version, &ent.Status, &ent.SupersededBy,
		&ent.Confidence, &ent.SourceType, &ent.ValidFrom, &ent.ValidUntil,
		(*int)(&ent.TaintLevel),
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, apperr.New(apperr.CodeNotFound, "Entity not found by ID")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "SemanticMem.GetEntityByID", err)
	}

	ent.ID = "entity:" + strconv.FormatInt(ent.DBID, 10)
	if len(propertiesJSON) > 0 {
		_ = json.Unmarshal(propertiesJSON, &ent.Properties)
	}
	if len(embeddingBytes) > 0 {
		ent.Embedding = bytesToFloat32s(embeddingBytes)
	}
	return &ent, nil
}
