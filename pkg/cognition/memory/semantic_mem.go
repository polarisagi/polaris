package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/pb"
)

// IntentSubmitter abstracts the MutationBus submission to avoid circular dependency.
type IntentSubmitter interface {
	Submit(ctx context.Context, intent *pb.MutationIntent) error
}

// DBAccessor allows fetching the underlying sql.DB from the store
type DBAccessor interface {
	DB() *sql.DB
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

func (sm *SemanticMem) StoreDocument(ctx context.Context, doc protocol.Document) error {
	key := []byte("doc:" + doc.ID)
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return sm.store.Put(ctx, key, data)
}

func (sm *SemanticMem) StoreChunks(ctx context.Context, docID string, chunks []protocol.Chunk) error {
	for _, ch := range chunks {
		key := []byte("chunk:" + ch.ID)
		data, err := json.Marshal(ch)
		if err != nil {
			return err
		}
		if err := sm.store.Put(ctx, key, data); err != nil {
			return err
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

func (sm *SemanticMem) GetDocument(ctx context.Context, id string) (*protocol.Document, error) {
	data, err := sm.store.Get(ctx, []byte("doc:"+id))
	if err != nil {
		return nil, err
	}
	var doc protocol.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

func (sm *SemanticMem) Archive(ctx context.Context, id string, reason string) error {
	doc, err := sm.GetDocument(ctx, id)
	if err != nil {
		return err
	}
	doc.Archived = true
	return sm.StoreDocument(ctx, *doc)
}

func (sm *SemanticMem) UpsertFact(ctx context.Context, entity protocol.Entity) error {
	payload, err := json.Marshal(entity)
	if err != nil {
		return err
	}
	intent := &pb.MutationIntent{
		Table:     "semantic_entities",
		Operation: "upsert",
		Payload:   payload,
	}
	if sm.bus != nil {
		return sm.bus.Submit(ctx, intent)
	}
	return perrors.New(perrors.CodeInternal, "MutationBus not configured in SemanticMem")
}

func (sm *SemanticMem) UpsertRelation(ctx context.Context, rel protocol.Relation) error {
	payload, err := json.Marshal(rel)
	if err != nil {
		return err
	}
	intent := &pb.MutationIntent{
		Table:     "semantic_relations",
		Operation: "upsert",
		Payload:   payload,
	}
	if sm.bus != nil {
		return sm.bus.Submit(ctx, intent)
	}
	return perrors.New(perrors.CodeInternal, "MutationBus not configured in SemanticMem")
}

func (sm *SemanticMem) GetEntity(ctx context.Context, entityType, name string) (*protocol.Entity, error) {
	db, err := sm.requireDB()
	if err != nil {
		return nil, err
	}

	const q = `SELECT id, name, entity_type, properties, embedding, source_event_id, version,
	            status, COALESCE(superseded_by, 0)
	            FROM semantic_entities WHERE entity_type = ? AND name = ?`
	row := db.QueryRowContext(ctx, q, entityType, name)

	var ent protocol.Entity
	var propertiesJSON []byte
	var embeddingBytes []byte

	err = row.Scan(&ent.DBID, &ent.Name, &ent.Type, &propertiesJSON, &embeddingBytes,
		&ent.SourceEventID, &ent.Version, &ent.Status, &ent.SupersededBy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, perrors.New(perrors.CodeNotFound, "Entity not found")
		}
		return nil, err
	}

	ent.ID = "entity:" + strconv.FormatInt(ent.DBID, 10)
	if len(propertiesJSON) > 0 {
		_ = json.Unmarshal(propertiesJSON, &ent.Properties)
	}
	return &ent, nil
}

// ListActiveEntities 返回指定类型的活跃（status='active'）实体列表，供 Jaccard 矛盾检测使用。
func (sm *SemanticMem) ListActiveEntities(ctx context.Context, entityType string, limit int) ([]protocol.Entity, error) {
	db, err := sm.requireDB()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}

	const q = `SELECT id, name, entity_type, properties, source_event_id, version,
	            status, COALESCE(superseded_by, 0)
	            FROM semantic_entities WHERE status='active' AND entity_type=?
	            ORDER BY updated_at DESC LIMIT ?`
	rows, err := db.QueryContext(ctx, q, entityType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []protocol.Entity
	for rows.Next() {
		var ent protocol.Entity
		var propertiesJSON []byte
		if err := rows.Scan(&ent.DBID, &ent.Name, &ent.Type, &propertiesJSON,
			&ent.SourceEventID, &ent.Version, &ent.Status, &ent.SupersededBy); err != nil {
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
		return err
	}
	var supersededBy any
	if newDBID > 0 {
		supersededBy = newDBID
	}
	_, err = db.ExecContext(ctx,
		`UPDATE semantic_entities SET status='superseded', superseded_by=?, updated_at=?
		 WHERE id=? AND status='active'`,
		supersededBy, time.Now().UnixMilli(), oldDBID)
	return err
}

// UpsertUserProfile 创建或更新用户画像。
func (sm *SemanticMem) UpsertUserProfile(ctx context.Context, profile protocol.UserProfile) error {
	db, err := sm.requireDB()
	if err != nil {
		return err
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
	return err
}

// GetUserProfile 读取用户画像，不存在返回 nil 和 CodeNotFound 错误。
func (sm *SemanticMem) GetUserProfile(ctx context.Context, profileKey string) (*protocol.UserProfile, error) {
	db, err := sm.requireDB()
	if err != nil {
		return nil, err
	}

	const q = `SELECT profile_key, stable_facts, recent_activity, behavioral_patterns,
	            synthesis_count, COALESCE(last_event_ts, 0)
	            FROM user_profile WHERE profile_key=?`
	row := db.QueryRowContext(ctx, q, profileKey)

	var p protocol.UserProfile
	var stableJSON, recentJSON, behavioralJSON []byte
	err = row.Scan(&p.ProfileKey, &stableJSON, &recentJSON, &behavioralJSON,
		&p.SynthesisCount, &p.LastEventTS)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, perrors.New(perrors.CodeNotFound, "user_profile not found")
		}
		return nil, err
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

func (sm *SemanticMem) requireDB() (*sql.DB, error) {
	dbAccess, ok := sm.store.(DBAccessor)
	if !ok {
		return nil, perrors.New(perrors.CodeInternal, "Store does not implement DBAccessor")
	}
	db := dbAccess.DB()
	if db == nil {
		return nil, perrors.New(perrors.CodeInternal, "Underlying DB is nil")
	}
	return db, nil
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
