package knowledge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/types"
)

// scoredRetriever 固定返回带分数的检索结果的 fake HybridRetriever。
type scoredRetriever struct{ frags []types.ScoredFragment }

func (r scoredRetriever) Search(context.Context, string, types.SearchScope, types.RetrievalConfig) ([]types.ScoredFragment, error) {
	return r.frags, nil
}

// ADR-0105 决策十：KnowledgeBase.Search 不得再丢弃检索器给出的分数——
// 下游召回融合按分排序、按分过滤，丢分等于给所有 RAG 命中填常量。
func TestKnowledgeBase_Search_PropagatesRetrieverScore(t *testing.T) {
	ret := scoredRetriever{frags: []types.ScoredFragment{
		{Source: "chunk:a", Content: "甲", Score: 0.91},
		{Source: "chunk:b", Content: "乙", Score: 0.37},
		{Source: "chunk:a", Content: "甲（重复）", Score: 0.05}, // 同一 chunk 重复命中：首次分为准
	}}
	kb := NewKnowledgeBase(ret, NewContextExpander(store.NewStorageRouter(&mockSqliteStore{}, nil)), nil, nil, nil, nil)

	res, err := kb.Search(context.Background(), KnowledgeBaseSearchRequest{Query: "q", TopK: 5, TaintMax: types.TaintHigh})
	require.NoError(t, err)
	require.Len(t, res, 2)
	require.Equal(t, "甲", res[0].Primary.Content)
	require.InDelta(t, 0.91, res[0].Score, 1e-9)
	require.Equal(t, "乙", res[1].Primary.Content)
	require.InDelta(t, 0.37, res[1].Score, 1e-9)
}
