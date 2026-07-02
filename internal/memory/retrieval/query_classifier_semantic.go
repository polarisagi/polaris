package retrieval

import (
	"context"
	"math"
	"sync"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// SemanticQueryClassifier 使用语义向量分类意图
type SemanticQueryClassifier struct {
	mu   sync.RWMutex
	vecs map[QueryType][]float32
}

// NewSemanticQueryClassifier 创建一个语义分类器
var GlobalSemanticClassifier = NewSemanticQueryClassifier()

func NewSemanticQueryClassifier() *SemanticQueryClassifier {
	return &SemanticQueryClassifier{}
}

// InitPrototypes 在 Tier-1+ 启动时调用一次，预计算 4 个原型向量并缓存。
// embedder 为 nil 时为 no-op（Tier-0 不调用此函数）。
func (s *SemanticQueryClassifier) InitPrototypes(ctx context.Context, embedder QueryEmbedder) error {
	if embedder == nil {
		return nil
	}
	// prototypeSeeds 是每种 QueryType 的原型种子句（中英混合，覆盖典型 query 模式）。
	prototypeSeeds := map[QueryType]string{
		QueryTypeTemporal:  "最近发生了什么？上周我说了什么？",
		QueryTypeFactual:   "什么是 transformer？定义一下 attention 机制。",
		QueryTypeHowTo:     "如何配置 Go 模块？请给出步骤。",
		QueryTypeReasoning: "为什么 Rust 比 C++ 更安全？分析原因。",
	}
	vecs := make(map[QueryType][]float32, len(prototypeSeeds))
	for qt, seed := range prototypeSeeds {
		v, err := embedder.Embed(ctx, seed)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "SemanticQueryClassifier.InitPrototypes", err)
		}
		vecs[qt] = v
	}
	s.mu.Lock()
	s.vecs = vecs
	s.mu.Unlock()
	return nil
}

// ClassifyQuerySemantic 使用余弦相似度将 query 分类至最近原型。
// confidence < 0.3 时回退 QueryTypeUnknown（M05 §4.3）。
// embedder 为 nil 或原型未初始化时降级为 ClassifyQuery（Tier-0 关键词路径）。
func (s *SemanticQueryClassifier) ClassifyQuerySemantic(ctx context.Context, query string, embedder QueryEmbedder) QueryType {
	s.mu.RLock()
	vecs := s.vecs
	s.mu.RUnlock()

	if embedder == nil || len(vecs) == 0 {
		// Tier-0 降级
		return ClassifyQuery(query)
	}

	qvec, err := embedder.Embed(ctx, query)
	if err != nil || len(qvec) == 0 {
		// embedding 失败 → 降级关键词路径（可用性优先）
		return ClassifyQuery(query)
	}

	var bestType = QueryTypeUnknown
	var bestSim float64 = -1

	for qt, pvec := range vecs {
		sim := cosineSimilaritySemantic(qvec, pvec)
		if sim > bestSim {
			bestSim = sim
			bestType = qt
		}
	}

	if bestSim < 0.3 {
		// 置信度不足，回退 unknown（M05 §4.3）
		return QueryTypeUnknown
	}
	return bestType
}

// cosineSimilaritySemantic 计算两个等长向量的余弦相似度，[−1, 1]。
// 零向量返回 0。
func cosineSimilaritySemantic(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
