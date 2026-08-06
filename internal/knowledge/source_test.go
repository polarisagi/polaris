package knowledge

import (
	"testing"

	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestChunkToFragment_FusionKeyVsOutputSource 守护 M10 检索的两个不同 Source 语义：
//   - 融合期必须以 **chunk ID** 为去重键（同一文档的不同分块是彼此独立的召回
//     单元；若按 SourceURI 聚合会被错误折叠成一条）；
//   - 对外输出必须还原为 **SourceURI**（调用方据此做引用溯源，重构前
//     toScoredFragments 即如此）。
//
// GD-13-002 下沉重构初版把两者合并成"Source=chunk ID"直接对外，引用溯源
// 静默退化为内部 ID。本测试即该回归的守护。
func TestChunkToFragment_FusionKeyVsOutputSource(t *testing.T) {
	src := &knowledgeDocumentSource{}
	f := src.chunkToFragment(Chunk{
		ID:         "chunk-abc",
		SourceURI:  "obsidian://vault/notes/go.md",
		Content:    "hello",
		TaintLevel: 2,
	})

	if f.Source != "chunk-abc" {
		t.Fatalf("fusion key must be chunk ID, got %q", f.Source)
	}
	if got := src.resolveURI(f.Source); got != "obsidian://vault/notes/go.md" {
		t.Fatalf("output Source must resolve back to SourceURI, got %q", got)
	}
	if f.TaintLevel != types.TaintLevel(2) {
		t.Fatalf("taint level must be carried through, got %d", f.TaintLevel)
	}
	// Score 刻意为 0：三路召回自身已排好序，融合层用 SliceStable 保留该次序。
	if f.Score != 0 {
		t.Fatalf("chunk fragments must keep Score=0 so stable sort preserves recall order, got %f", f.Score)
	}
}

// TestResolveURI_UnregisteredPassthrough 未登记的伪 ID（宏观 Community 摘要）
// 必须原样返回，不得被静默丢弃或置空。
func TestResolveURI_UnregisteredPassthrough(t *testing.T) {
	src := &knowledgeDocumentSource{}
	if got := src.resolveURI("macro-community-7"); got != "macro-community-7" {
		t.Fatalf("unregistered id must pass through, got %q", got)
	}
}

// TestKnowledgeWeights_MatchM10Spec 权重必须锁死为 M10 §2.2 的
// BM25×0.3 + Vector×0.6 + Graph×0.1，不得退化为 M5 的默认权重体系。
// 重构初版误传 config.*Weight，在调用方未显式设置时会被融合层补成
// 1.0/0.6/0.6，把 M10 的检索策略悄悄改成 M5 的。
func TestKnowledgeWeights_MatchM10Spec(t *testing.T) {
	if knowledgeBM25Weight != 0.3 || knowledgeVectorWeight != 0.6 || knowledgeGraphWeight != 0.1 {
		t.Fatalf("M10 §2.2 weights drifted: bm25=%v vector=%v graph=%v",
			knowledgeBM25Weight, knowledgeVectorWeight, knowledgeGraphWeight)
	}
	if knowledgeRRFk != 60 {
		t.Fatalf("RRF k must stay 60, got %d", knowledgeRRFk)
	}
}

// 编译期断言：knowledgeDocumentSource 必须满足统一融合管线的接口。
var _ search.DocumentSource = (*knowledgeDocumentSource)(nil)
