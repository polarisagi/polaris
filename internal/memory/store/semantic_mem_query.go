package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/polarisagi/polaris/internal/memory/util"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ListActiveEntities 返回指定类型的活跃（status='active'）实体列表，供 Jaccard 矛盾检测使用。
func (sm *SemanticMem) ListActiveEntities(ctx context.Context, entityType string, limit int, asOf int64) ([]types.Entity, error) {
	db, err := sm.requireDB()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SemanticMem.ListActiveEntities", err)
	}
	if limit <= 0 {
		limit = 50
	}
	if asOf <= 0 {
		asOf = time.Now().UnixMilli()
	}

	const q = `SELECT id, name, entity_type, properties,
	            COALESCE(source_event_id, 0), version,
	            status, COALESCE(superseded_by, 0),
	            confidence, source_type,
	            COALESCE(valid_from, 0), COALESCE(valid_until, 0)
	            FROM semantic_entities 
	            WHERE status='active' AND entity_type=? 
	              AND (valid_from <= ? OR valid_from IS NULL OR valid_from = 0) 
	              AND (valid_until > ? OR valid_until IS NULL OR valid_until = 0)
	            ORDER BY updated_at DESC LIMIT ?`
	rows, err := db.QueryContext(ctx, q, entityType, asOf, asOf, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SemanticMem.ListActiveEntities", err)
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
		// L3：反序列化失败时 Properties 保持零值继续返回，下游按"无扩展属性"处理。
		if len(propertiesJSON) > 0 {
			if err := json.Unmarshal(propertiesJSON, &ent.Properties); err != nil {
				slog.Warn("memory/semantic_mem: properties 反序列化失败，实体属性按空处理", "entity_id", ent.DBID, "err", err)
				metrics.RecordMemoryJSONDecodeFailure(ctx, "semantic_entities.properties")
			}
		}
		result = append(result, ent)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SemanticMem.ListActiveEntities: rows iteration", err)
	}
	return result, nil
}

// MarkEntitySuperseded 将旧实体标记为 superseded，并记录取代它的新实体 DBID。
// 仅当当前 status='active' 时生效（防重复超越）。
func (sm *SemanticMem) MarkEntitySuperseded(ctx context.Context, oldDBID int64, newDBID int64) error {
	db, err := sm.requireDB()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SemanticMem.MarkEntitySuperseded", err)
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
		return apperr.Wrap(apperr.CodeInternal, "SemanticMem.MarkEntitySuperseded", err)
	}

	if sm.cognitive != nil {
		if oldEnt, err := sm.GetEntityByID(ctx, oldDBID); err == nil {
			_ = sm.cognitive.FTSDelete("sement_" + oldEnt.Type + "_" + oldEnt.Name)
		}
	}

	return nil
}

// MarkEntityExpired 主动将指定实体标记为过期。
func (sm *SemanticMem) MarkEntityExpired(ctx context.Context, entityType, name, reason string) error {
	db, err := sm.requireDB()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SemanticMem.MarkEntityExpired", err)
	}

	_, err = db.ExecContext(ctx,
		`UPDATE semantic_entities SET status='expired', updated_at=?
		 WHERE entity_type=? AND name=? AND status='active'`,
		time.Now().UnixMilli(), entityType, name)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SemanticMem.MarkEntityExpired", err)
	}

	if sm.cognitive != nil {
		_ = sm.cognitive.FTSDelete("sement_" + entityType + "_" + name)
	}

	return nil
}

// UpsertUserProfile 创建或更新用户画像。
func (sm *SemanticMem) UpsertUserProfile(ctx context.Context, profile types.UserProfile) error {
	db, err := sm.requireDB()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "SemanticMem.UpsertUserProfile", err)
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
		return apperr.Wrap(apperr.CodeInternal, "SemanticMem.UpsertUserProfile", err)
	}
	return nil
}

// GetUserProfile 读取用户画像，不存在返回 nil 和 CodeNotFound 错误。
func (sm *SemanticMem) GetUserProfile(ctx context.Context, profileKey string) (*types.UserProfile, error) {
	db, err := sm.requireDB()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SemanticMem.GetUserProfile", err)
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
		return nil, apperr.Wrap(apperr.CodeInternal, "SemanticMem.GetUserProfile", err)
	}
	// L3：三个字段各自独立反序列化，任一失败仅令该字段保持零值（nil），不影响
	// profile_key/synthesis_count 等核心字段，画像合成侧按"该维度缺失"处理。
	if len(stableJSON) > 0 {
		if err := json.Unmarshal(stableJSON, &p.StableFacts); err != nil {
			slog.Warn("memory/semantic_mem: user_profile.stable_facts 反序列化失败", "profile_key", profileKey, "err", err)
			metrics.RecordMemoryJSONDecodeFailure(ctx, "user_profile.stable_facts")
		}
	}
	if len(recentJSON) > 0 {
		if err := json.Unmarshal(recentJSON, &p.RecentActivity); err != nil {
			slog.Warn("memory/semantic_mem: user_profile.recent_activity 反序列化失败", "profile_key", profileKey, "err", err)
			metrics.RecordMemoryJSONDecodeFailure(ctx, "user_profile.recent_activity")
		}
	}
	if len(behavioralJSON) > 0 {
		if err := json.Unmarshal(behavioralJSON, &p.BehavioralPatterns); err != nil {
			slog.Warn("memory/semantic_mem: user_profile.behavioral_patterns 反序列化失败", "profile_key", profileKey, "err", err)
			metrics.RecordMemoryJSONDecodeFailure(ctx, "user_profile.behavioral_patterns")
		}
	}
	return &p, nil
}

// ─── 内部辅助 ─────────────────────────────────────────────────────────────────

func (sm *SemanticMem) requireDB() (protocol.SQLQuerier, error) {
	if q, ok := sm.store.(protocol.SQLQuerier); ok && q != nil {
		return q, nil
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

// SearchEntities 按关键词检索活跃语义实体（第 6 路检索的数据源）。
// SQL LIKE 宽召回（上限 100 条）→ Go 侧 BM25 精排 → 截断 limit。
func (sm *SemanticMem) SearchEntities(ctx context.Context, query string, limit int, asOf int64) ([]types.Entity, error) {
	db, err := sm.requireDB()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SemanticMem.SearchEntities", err)
	}

	if asOf <= 0 {
		asOf = time.Now().UnixMilli()
	}

	likeQ := "%" + query + "%"
	const sqlQ = `SELECT id, name, entity_type, properties, embedding,
	            COALESCE(source_event_id, 0), version,
	            status, COALESCE(superseded_by, 0),
	            confidence, source_type,
	            COALESCE(valid_from, 0), COALESCE(valid_until, 0),
	            COALESCE(taint_level, 0)
	            FROM semantic_entities
	            WHERE status='active' 
	              AND source_type IN ('llm_extract', 'user_stated', 'agent_inferred', 'graphrag_ingest')
	              AND (name LIKE ? OR properties LIKE ?)
	              AND (valid_from <= ? OR valid_from IS NULL OR valid_from = 0) 
	              AND (valid_until > ? OR valid_until IS NULL OR valid_until = 0)
	            LIMIT 100` // 取100条再 bm25

	rows, err := db.QueryContext(ctx, sqlQ, likeQ, likeQ, asOf, asOf)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "SemanticMem.SearchEntities", err)
	}
	defer rows.Close()

	var results []types.Entity
	for rows.Next() {
		var ent types.Entity
		var propertiesJSON []byte
		var embeddingBytes []byte

		err = rows.Scan(
			&ent.DBID, &ent.Name, &ent.Type, &propertiesJSON, &embeddingBytes,
			&ent.SourceEventID, &ent.Version, &ent.Status, &ent.SupersededBy,
			&ent.Confidence, &ent.SourceType, &ent.ValidFrom, &ent.ValidUntil,
			(*int)(&ent.TaintLevel),
		)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "SemanticMem.SearchEntities", err)
		}
		ent.ID = "entity:" + strconv.FormatInt(ent.DBID, 10)
		// L3：反序列化失败时 Properties 保持零值继续返回，下游按"无扩展属性"处理。
		if len(propertiesJSON) > 0 {
			if err := json.Unmarshal(propertiesJSON, &ent.Properties); err != nil {
				slog.Warn("memory/semantic_mem: properties 反序列化失败，实体属性按空处理", "entity_id", ent.DBID, "err", err)
				metrics.RecordMemoryJSONDecodeFailure(ctx, "semantic_entities.properties")
			}
		}
		if len(embeddingBytes) > 0 {
			ent.Embedding = bytesToFloat32s(embeddingBytes)
		}
		results = append(results, ent)
	}

	// bm25 排序
	type scoredEnt struct {
		ent   types.Entity
		score float64
	}
	var scored []scoredEnt
	for _, e := range results {
		var propStr string
		if b, err := json.Marshal(e.Properties); err == nil {
			propStr = string(b)
		}
		text := e.Name + " " + propStr
		s := util.Bm25Score(query, text)
		scored = append(scored, scoredEnt{ent: e, score: s})
	}
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})
	if len(scored) > limit && limit > 0 {
		scored = scored[:limit]
	}
	out := make([]types.Entity, len(scored))
	for i, v := range scored {
		out[i] = v.ent
	}
	return out, nil
}

func bytesToFloat32s(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	res := make([]float32, len(b)/4)
	for i := 0; i < len(res); i++ {
		res[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4 : i*4+4]))
	}
	return res
}
