package graphrag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
)

// GraphWriter 负责将实体写入数据库，通过 MutationBus 实现单写者串行化，
// 并在写入前执行实体消歧（基于余弦相似度，保留 version 高者）。
// 架构文档: docs/arch/M10-Knowledge-RAG.md §2.8
type GraphWriter struct {
	bus     *store.DatabaseWriter
	fetcher EntityFetcher
}

func NewGraphWriter(bus *store.DatabaseWriter, fetcher EntityFetcher) *GraphWriter {
	return &GraphWriter{bus: bus, fetcher: fetcher}
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

	intent := &store.MutationIntent{
		Table:          "entities",
		Operation:      "upsert",
		Key:            []byte(e.Name),
		Payload:        []byte(e.ID),
		ClaimedVersion: e.SyncVersion,
	}
	return gw.bus.Submit(ctx, intent)
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
	//nolint:bare-infer // 历史代码暂留，后续重构替换
	resp, err := pc.provider.Infer(ctx, req.Messages, types.WithMaxTokens(req.MaxTokens))
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
	//nolint:bare-infer // 历史代码暂留，后续重构替换
	resp, err := pc.provider.Infer(ctx, req.Messages, types.WithMaxTokens(req.MaxTokens))
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
