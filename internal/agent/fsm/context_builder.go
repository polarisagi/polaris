package fsm

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

// CogResult L2 语义检索的单条结果。
type CogResult struct {
	DocID   string
	Snippet string
	Score   float32
	// Taint 命中内容的污点等级（召回按 MaxTaint 过滤、并据此给召回段定级）。
	Taint types.TaintLevel
}

// CognitiveSearcher L2 语义检索接口（消费方定义，防止包循环）。
//
// 只有 FTS 一路：SurrealDB 向量索引里只有情景事件与扩展目录的向量，语义实体（L2 的唯一独有数据）
// 只写 FTS 索引（SemanticMem.UpsertFact），没有向量可查，故不设 VecKNN（ADR-0105 决策十 WP8 追记）。
type CognitiveSearcher interface {
	FTSSearch(ctx context.Context, query string, k int) ([]CogResult, error)
}

// KnowledgeResult 单条 RAG 命中。Score 是检索器给的分（仅用于同一次检索内排序，不可跨来源比较）。
type KnowledgeResult struct {
	Content string
	Source  string
	Score   float32
	Taint   types.TaintLevel
}

type KnowledgeSearcher interface {
	SearchRAG(ctx context.Context, query string, topK int) ([]KnowledgeResult, error)
}

// RecallReranker 召回相关度门的重排器（消费方私有接口，R1.4；ADR-0105 决策十）。
// 实现必须是本地推理（零 API token）；返回值是校准后的相关**概率** [0,1]，长度与 docs 一致。
// 不可用时调用方不注入（nil），门被跳过。
type RecallReranker interface {
	RelevanceProbs(ctx context.Context, query string, docs []string) ([]float64, error)
}

// ContextBuilder 接口由使用状态机的客户端（如 agent）实现，
// 用于在状态机执行时注入 Prompt 上下文组装能力。
type ContextBuilder interface {
	BuildPerceiveContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *StateContext, cognitive CognitiveSearcher) ([]types.Message, error)
	BuildPlanContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *StateContext, cata catalog.Catalog, cognitive CognitiveSearcher) ([]types.Message, error)
	BuildReflectContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *StateContext) ([]types.Message, error)
	// BuildRespondContext 组装 S_RESPOND（唯一面向用户的阶段，ADR-0098）的 prompt。
	BuildRespondContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *StateContext) ([]types.Message, error)
	// BuildToolListSection 返回工具目录正文及该次目录中出现过的最高来源污点等级
	// （S-02：MCP 外部工具描述必须按来源分级，供调用方决定是否 Spotlighting）。
	BuildToolListSection(ctx context.Context, cata catalog.Catalog) (string, types.TaintLevel)
}
