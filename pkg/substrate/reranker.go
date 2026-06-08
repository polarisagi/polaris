package substrate

import (
	"context"
	"math"
	"strings"
)

// Reranker 后处理重排接口。NilReranker 是默认零值实现（透传）。
//
// 架构文档：docs/arch/10-Knowledge-RAG-深度选型.md §2.2（Late-Interaction 神经重排）
// 前置条件：ColBERT 模型需 onnxruntime-go 生产就绪后接入；
//
//	在此之前默认使用 NilReranker 透传（不影响 RRF 排序结果）。
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []ScoredFragment) []ScoredFragment
}

// NilReranker 透传重排器（默认），不修改文档顺序。
type NilReranker struct{}

func (NilReranker) Rerank(_ context.Context, _ string, docs []ScoredFragment) []ScoredFragment {
	return docs
}

// MaxSimScore 计算 ColBERT Late-Interaction MaxSim 分数。
//
// ColBERT 公式：S(Q,D) = Σ_{q∈Q} max_{d∈D} cosine(q, d) / |Q|
//
//   - qVecs: query token embeddings，shape [qLen][dim]
//   - dVecs: document token embeddings，shape [dLen][dim]
//
// 返回 [0, 1] 分数；empty input 返回 0。
func MaxSimScore(qVecs, dVecs [][]float32) float64 {
	if len(qVecs) == 0 || len(dVecs) == 0 {
		return 0
	}
	total := 0.0
	for _, qv := range qVecs {
		maxSim := -1.0
		for _, dv := range dVecs {
			if s := cosineSim32(qv, dv); s > maxSim {
				maxSim = s
			}
		}
		if maxSim > -1 {
			total += maxSim
		}
	}
	return total / float64(len(qVecs))
}

// cosineSim32 float32 向量余弦相似度，输入长度不匹配时返回 0。
func cosineSim32(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, n1, n2 float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		n1 += ai * ai
		n2 += bi * bi
	}
	denom := math.Sqrt(n1) * math.Sqrt(n2)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

// ApproximateColBERTReranker 近似 ColBERT 重排器。
//
// 不依赖单独的 ColBERT ONNX 模型；使用现有 Embedder 将文本切分为
// 重叠 n-gram 窗口，模拟 token 级别的 MaxSim 计算。
//
// 性能：O(qWindows × dWindows × dim)，适合 RerankTopM ≤ 50 的场景。
// 精度：相较全精度 ColBERT 有损失，但在 Tier-0 下是最优权衡。
//
// 升级路径：当 onnxruntime-go 生产就绪时，替换为 ColBERTONNXReranker
// （从同一接口接入，调用方无需改动）。
type ApproximateColBERTReranker struct {
	embedder Embedder
	window   int // n-gram 窗口大小（词），默认 3
}

// NewApproximateColBERTReranker 构造近似 ColBERT 重排器。
// window=0 时使用默认值 3；embedder 不能为 nil。
func NewApproximateColBERTReranker(embedder Embedder, window int) *ApproximateColBERTReranker {
	if window <= 0 {
		window = 3
	}
	return &ApproximateColBERTReranker{embedder: embedder, window: window}
}

// Rerank 对 docs 按 MaxSim 分数降序重排，返回新切片（不修改输入）。
func (r *ApproximateColBERTReranker) Rerank(_ context.Context, query string, docs []ScoredFragment) []ScoredFragment {
	if len(docs) == 0 || r.embedder == nil {
		return docs
	}

	qVecs := r.tokenVecs(query)
	if len(qVecs) == 0 {
		return docs
	}

	result := make([]scoredDoc, len(docs))
	for i, d := range docs {
		dVecs := r.tokenVecs(d.Content)
		result[i] = scoredDoc{doc: d, score: MaxSimScore(qVecs, dVecs)}
	}

	// 降序排序（稳定）
	stableSort(result)

	out := make([]ScoredFragment, len(result))
	for i, s := range result {
		frag := s.doc
		frag.Score = s.score // 用 MaxSim 分数替换 RRF 分数
		out[i] = frag
	}
	return out
}

// tokenVecs 将文本切分为重叠 n-gram 窗口，每窗口调用 Embedder 得到向量。
func (r *ApproximateColBERTReranker) tokenVecs(text string) [][]float32 {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}

	var vecs [][]float32
	step := max(1, r.window/2) // 步长 = window/2（50% 重叠）
	for i := 0; i < len(words); i += step {
		end := min(i+r.window, len(words))
		chunk := strings.Join(words[i:end], " ")
		if v := r.embedder.Embed(chunk); len(v) > 0 {
			vecs = append(vecs, v)
		}
	}
	return vecs
}

// stableSort 降序稳定排序（插入排序，n ≤ 50 时足够）。
type scoredDoc struct {
	doc   ScoredFragment
	score float64
}

func stableSort(s []scoredDoc) {
	for i := 1; i < len(s); i++ {
		key := s[i]
		j := i - 1
		for j >= 0 && s[j].score < key.score {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = key
	}
}
