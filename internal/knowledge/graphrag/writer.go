package graphrag

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
)

// GraphWriter 负责将实体写入数据库，通过 MutationBus 实现单写者串行化，
// 并在写入前执行实体消歧（基于余弦相似度，保留 version 高者）。
// 架构文档: docs/arch/M10-Knowledge-RAG.md §2.8
type GraphWriter struct {
	bus        *store.DatabaseWriter
	fetcher    EntityFetcher
	semanticDB protocol.SQLQuerier // B2: 桥接检查 semantic_entities
}

// SetSemanticDB 注入语义记忆底层 DB 以便写入期去重。
func (gw *GraphWriter) SetSemanticDB(db protocol.SQLQuerier) {
	gw.semanticDB = db
}

// UpsertEntity 提交实体写入意图。写入前通过余弦相似度消歧，LWW 语义保留 version 较高者。
func (gw *GraphWriter) UpsertEntity(ctx context.Context, e *Entity) error {
	if gw.fetcher != nil {
		existing, err := gw.fetcher.GetEntityByName(ctx, e.Name)
		if err == nil && existing != nil {
			sim := CosineSimilarity(existing.Embedding, e.Embedding)
			if sim > 0.95 && e.SyncVersion <= existing.SyncVersion {
				return nil
			}
		}
	}

	// B2: 同样检查 semantic_entities 侧是否存在同名同类型高相似度实体
	if skip := gw.upsertToSemanticDB(ctx, e); skip {
		return nil
	}

	intent := &store.MutationIntent{
		Table:          "entities",
		Operation:      "upsert",
		Key:            []byte(e.Name),
		Payload:        []byte(e.ID),
		ClaimedVersion: e.SyncVersion,
	}
	return gw.bus.Submit(ctx, intent)
}

// upsertToSemanticDB 向 semantic_entities 写入 graphrag_ingest 来源的实体。
// 返回 true 表示已被高相似度低版本实体去重跳过（调用方应跳过后续写入）。
func (gw *GraphWriter) upsertToSemanticDB(ctx context.Context, e *Entity) (skip bool) {
	if gw.semanticDB == nil {
		return false
	}
	var existingEmbedding []byte
	var existingVersion int64
	var dbid int64
	err := gw.semanticDB.QueryRowContext(ctx, "SELECT id, embedding, version FROM semantic_entities WHERE entity_type = ? AND name = ?", e.Type, e.Name).Scan(&dbid, &existingEmbedding, &existingVersion)
	if err == nil && len(existingEmbedding) > 0 {
		embFloats := bytesToFloat32s(existingEmbedding)
		sim := CosineSimilarity(embFloats, e.Embedding)
		if sim > 0.95 && e.SyncVersion <= existingVersion {
			return true // 高相似度低版本：跳过
		}
		// Update existing entity in semantic_entities if it exists but version is higher or sim is low
		_, _ = gw.semanticDB.ExecContext(ctx, `UPDATE semantic_entities SET embedding = ?, version = ?, source_type = 'graphrag_ingest', updated_at = strftime('%s','now')*1000 WHERE id = ?`, float32sToBytes(e.Embedding), e.SyncVersion, dbid)
	} else {
		// Insert new entity into semantic_entities
		_, _ = gw.semanticDB.ExecContext(ctx, `INSERT INTO semantic_entities (entity_type, name, properties, embedding, version, source_type, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 'graphrag_ingest', strftime('%s','now')*1000, strftime('%s','now')*1000)`, e.Type, e.Name, "{}", float32sToBytes(e.Embedding), e.SyncVersion)
	}
	return false
}

func float32sToBytes(f []float32) []byte {
	if len(f) == 0 {
		return nil
	}
	b := make([]byte, len(f)*4)
	for i, v := range f {
		bits := math.Float32bits(v)
		b[i*4] = byte(bits)
		b[i*4+1] = byte(bits >> 8)
		b[i*4+2] = byte(bits >> 16)
		b[i*4+3] = byte(bits >> 24)
	}
	return b
}

func bytesToFloat32s(b []byte) []float32 {
	if len(b)%4 != 0 {
		return nil
	}
	floats := make([]float32, len(b)/4)
	for i := range floats {
		bits := uint32(b[i*4]) | uint32(b[i*4+1])<<8 | uint32(b[i*4+2])<<16 | uint32(b[i*4+3])<<24
		floats[i] = math.Float32frombits(bits)
	}
	return floats
}

// ---------------------------------------------------------------------------
// LLMClient LLM 调用接口（图构建专用）。

type LLMClient interface {
	ExtractEntities(ctx context.Context, text string) ([]*Entity, error)
	ExtractRelations(ctx context.Context, entities []*Entity, text string) ([]*Relation, error)
}

// ProviderLLMClient 基于 protocol.Provider 的 LLMClient 实现。
// 使用 DeepSeek API 做实体/关系提取，成本极低（¥1-3/1M tokens）。
type ProviderLLMClient struct {
	provider protocol.Provider
	model    string
}

func NewProviderLLMClient(provider protocol.Provider, model string) *ProviderLLMClient {
	return &ProviderLLMClient{provider: provider, model: model}
}

// ExtractEntities 调用 LLM 从文本中提取实体列表。
func (pc *ProviderLLMClient) ExtractEntities(ctx context.Context, text string) ([]*Entity, error) {
	prompt := fmt.Sprintf(
		"Extract all named entities from the following text. "+
			"Return a JSON array of objects with keys: name, type (one of: person, project, tool, concept, file, version, domain). "+
			"Only return the JSON array, no other text.\n\nText:\n%s",
		truncate(text, 4000),
	)
	req := &types.InferRequest{
		Messages:    []types.Message{{Role: "user", Content: prompt}},
		MaxTokens:   1024,
		Temperature: 0.1,
	}
	if pc.model != "" {
		req.Model = pc.model
	}
	resp, err := safecall.Infer(ctx, pc.provider, req.Messages, types.WithMaxTokens(req.MaxTokens))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("LLM entity extraction failed: %v", err), err)
	}
	return parseEntityJSON(resp.Content)
}

// ExtractRelations 调用 LLM 从文本+实体列表中提取关系。
func (pc *ProviderLLMClient) ExtractRelations(ctx context.Context, entities []*Entity, text string) ([]*Relation, error) {
	entityNames := make([]string, len(entities))
	for i, e := range entities {
		entityNames[i] = e.Name
	}
	prompt := fmt.Sprintf(
		"Given these entities: %s\n\nAnd this text:\n%s\n\n"+
			"Identify relationships between entities. Return a JSON array of objects with keys: "+
			"from (entity name), to (entity name), type (one of: uses, depends_on, configures, extends, contradicts, replaces, version_of). "+
			"Only return the JSON array, no other text.",
		strings.Join(entityNames, ", "), truncate(text, 3000),
	)
	req := &types.InferRequest{
		Messages:    []types.Message{{Role: "user", Content: prompt}},
		MaxTokens:   1024,
		Temperature: 0.1,
	}
	if pc.model != "" {
		req.Model = pc.model
	}
	resp, err := safecall.Infer(ctx, pc.provider, req.Messages, types.WithMaxTokens(req.MaxTokens))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("LLM relation extraction failed: %v", err), err)
	}
	return parseRelationJSON(resp.Content)
}

// ---------------------------------------------------------------------------
// helpers

func truncate(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "..."
}

func parseEntityJSON(content string) ([]*Entity, error) {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var raw []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("parse entity JSON: %v", err), err)
	}
	entities := make([]*Entity, len(raw))
	for i, r := range raw {
		entities[i] = &Entity{ID: r.Name, Name: r.Name, Type: r.Type}
	}
	return entities, nil
}

func parseRelationJSON(content string) ([]*Relation, error) {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var raw []struct {
		From string `json:"from"`
		To   string `json:"to"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("parse relation JSON: %v", err), err)
	}
	relations := make([]*Relation, len(raw))
	for i, r := range raw {
		relations[i] = &Relation{
			FromEntityID: r.From,
			ToEntityID:   r.To,
			RelationType: r.Type,
			Confidence:   0.85,
		}
	}
	return relations, nil
}
