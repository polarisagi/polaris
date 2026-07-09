package fsm

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

// CognitiveSearcher L2 语义检索接口（消费方定义，防止包循环）。

type CogResult struct {
	DocID   string
	Snippet string
	Score   float32
}

type CognitiveSearcher interface {
	FTSSearch(query string, k int) ([]CogResult, error)
	VecKNN(embedding []float32, k int) ([]CogResult, error)
}

type KnowledgeResult struct {
	Content string
	Source  string
	Score   float32
}

type KnowledgeSearcher interface {
	SearchRAG(ctx context.Context, query string, topK int) ([]KnowledgeResult, error)
}

// ContextBuilder 接口由使用状态机的客户端（如 agent）实现，
// 用于在状态机执行时注入 Prompt 上下文组装能力。
type ContextBuilder interface {
	BuildPerceiveContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *StateContext, cognitive CognitiveSearcher) ([]types.Message, error)
	BuildPlanContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *StateContext, cata catalog.Catalog, cognitive CognitiveSearcher) ([]types.Message, error)
	BuildReflectContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *StateContext) ([]types.Message, error)
	BuildToolListSection(ctx context.Context, cata catalog.Catalog) string
}
