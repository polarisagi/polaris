package agentctx

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// 召回单一管线的融合/相关度门/装入（ADR-0105 决策十）。全部是纯函数（重排门除外，它只调注入的本地重排器），
// 同输入字节一致。

// recallRRFK RRF 平滑常数（Cormack 等原文与全仓 m5_memory.rrf_k 同值）。k 越大，秩差对分数的影响越小。
const recallRRFK = 60

// recallWeights 来源权重（m4_kernel.recall.weight_*）。画像不参与融合，无权重。
type recallWeights struct{ reflection, episodic, semantic, rag float64 }

func recallWeightsOf(th config.M4KernelThresholds) recallWeights {
	return recallWeights{
		reflection: th.RecallWeightReflection,
		episodic:   th.RecallWeightEpisodic,
		semantic:   th.RecallWeightSemantic,
		rag:        th.RecallWeightRAG,
	}
}

func (w recallWeights) of(kind fsm.RecallKind) float64 {
	switch kind {
	case fsm.RecallReflection:
		return w.reflection
	case fsm.RecallEpisodic:
		return w.episodic
	case fsm.RecallSemantic:
		return w.semantic
	case fsm.RecallRAG:
		return w.rag
	case fsm.RecallProfile:
		return 0
	}
	return 0
}

// fusedItem 融合后的一条召回。
type fusedItem struct {
	kind  fsm.RecallKind // 渲染分组：取对该条贡献最大的来源
	item  fsm.RecallItem
	score float64 // 各来源 RRF 贡献之和
	best  float64 // 单来源最大贡献（决定同正文多来源时的归属）
	order int     // 首次出现序，末级 tiebreak
	kinds uint32  // 已计入的来源位图，防同来源同正文重复累加
}

// fuseRecall 各来源内已按原生分排序；这里以秩（不用分数）做加权 RRF 融合为单一序列：
//
//	score(d) = Σ_source weight_source / (k + rank_source(d))
//
// RRF 只用秩，不要求各来源分数可比（BM25 无界、余弦有界、RAG 是检索融合分）。同正文出现在多个来源时
// 分数累加（被多路独立命中的条目更可信）并只保留一条。已被 L2 历史包含的条目先剔除再排秩——
// 它已在缓存前缀里，放进 L4 是纯增量成本。
// 同分按 来源序、首次出现序 打破，保证确定性。权重 ≤0 的来源整体停用。
func fuseRecall(r *fsm.TurnRecall, history string, w recallWeights) []fusedItem {
	if r == nil {
		return nil
	}
	var out []*fusedItem
	index := make(map[string]int)
	for _, kind := range fsm.RecallKinds() {
		wt := w.of(kind)
		if wt <= 0 {
			continue
		}
		rank := 0
		for _, it := range r.Items[kind] {
			if it.Key == "" || containedInHistory(it.Key, history) {
				continue
			}
			rank++
			contrib := wt / float64(recallRRFK+rank)
			if idx, ok := index[it.Key]; ok {
				f := out[idx]
				if f.kinds&(1<<uint(kind)) != 0 {
					continue
				}
				f.kinds |= 1 << uint(kind)
				f.score += contrib
				if contrib > f.best {
					f.kind, f.item, f.best = kind, it, contrib
				}
				continue
			}
			index[it.Key] = len(out)
			out = append(out, &fusedItem{kind: kind, item: it, score: contrib, best: contrib, order: len(out), kinds: 1 << uint(kind)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		return a.order < b.order
	})
	res := make([]fusedItem, len(out))
	for i, f := range out {
		res[i] = *f
	}
	return res
}

// containedInHistory 条目正文（规范化空白后，≥minContainKeyRunes）被 L2 历史包含。
// 被截断的条目以 … 结尾，去掉后再判（截断前缀仍是历史的子串则同样重复）。
func containedInHistory(key, history string) bool {
	probe := strings.TrimSuffix(key, "…")
	return runeCount(probe) >= minContainKeyRunes && strings.Contains(history, probe)
}

// rerankGate 校准相关度门的参数。reranker 为 nil、topN≤0 或 query 为空时门不生效。
type rerankGate struct {
	reranker fsm.RecallReranker
	topN     int
	minProb  float64
	query    string
}

func (g rerankGate) active() bool { return g.reranker != nil && g.topN > 0 && g.query != "" }

// applyRerankGate 对融合后前 topN 条用本地重排器重排，丢弃相关概率 < minProb 的条目（并丢弃 topN 之外的
// 未验证尾部——门生效时只有通过验证的条目才配占预算）。重排失败/超时/返回非法概率一律跳过此门、
// 原样返回融合结果：召回是增益，门是精度优化，都不得阻断回合。
// 超时复用召回的时间预算（ctx 由调用方带截止）。
func applyRerankGate(ctx context.Context, g rerankGate, fused []fusedItem) []fusedItem {
	if !g.active() || len(fused) == 0 {
		return fused
	}
	cand := fused
	if len(cand) > g.topN {
		cand = cand[:g.topN]
	}
	docs := make([]string, len(cand))
	for i, c := range cand {
		docs[i] = c.item.Key
	}
	start := time.Now()
	probs, err := rerankWithin(ctx, g.reranker, g.query, docs)
	if err == nil {
		err = validateProbs(probs, len(cand))
	}
	ms := float64(time.Since(start).Milliseconds())
	if err != nil {
		outcome := "recall_gate_error"
		if apperr.IsCode(err, apperr.CodeTimeout) {
			outcome = "recall_gate_timeout"
		}
		metrics.RecordRerankCall(ctx, outcome, ms)
		slog.Warn("agentctx: recall rerank gate skipped", "outcome", outcome, "err", err)
		return fused
	}
	metrics.RecordRerankCall(ctx, "recall_gate_success", ms)
	kept := make([]fusedItem, 0, len(cand))
	for i, c := range cand {
		if probs[i] >= g.minProb {
			kept = append(kept, c)
		}
	}
	return kept
}

func validateProbs(probs []float64, want int) error {
	if len(probs) != want {
		return apperr.New(apperr.CodeInternal, "agentctx: reranker returned wrong number of scores")
	}
	for _, p := range probs {
		if math.IsNaN(p) || p < 0 || p > 1 {
			return apperr.New(apperr.CodeInternal, "agentctx: reranker returned a probability outside [0,1]")
		}
	}
	return nil
}

// rerankWithin 在 ctx 截止前等待重排完成。FFI 推理不可取消，故与 recallWithin 同样在边界放弃等待；
// 后台 goroutine 随推理返回自然退出，结果写入容量 1 的通道后被丢弃。
func rerankWithin(ctx context.Context, r fsm.RecallReranker, query string, docs []string) ([]float64, error) {
	type result struct {
		probs []float64
		err   error
	}
	done := make(chan result, 1)
	concurrent.SafeGo(ctx, "agentctx.recall_rerank", func(gctx context.Context) {
		p, err := r.RelevanceProbs(gctx, query, docs)
		done <- result{p, err}
	})
	select {
	case res := <-done:
		return res.probs, res.err
	case <-ctx.Done():
		return nil, apperr.Wrap(apperr.CodeTimeout, "agentctx: recall rerank exceeded budget", ctx.Err())
	}
}

// packRecall 按融合秩把条目装入 maxTokens 预算，按来源分组输出（便于模型理解）：组顺序固定为
// 反思、情景、L2、RAG，组内保持融合秩序；遇到第一条装不下的条目即止（严格按秩截断，不跳过它去塞
// 后面更短的低秩条目）。画像不参与融合：它与查询无关、无相关度可言，排在最后只填剩余预算。
// 同时返回已装入条目的最高污点——调用方据此给整段定级（污点只升不降）。
func packRecall(fused []fusedItem, profile []fsm.RecallItem, history string, maxTokens int) (string, types.TaintLevel) {
	kinds := fsm.RecallKinds()
	groups := make(map[fsm.RecallKind][]string, len(kinds))
	taint := types.TaintNone
	used := 0
	seen := make(map[string]struct{}, len(fused))
	for _, f := range fused {
		need := estimateTokens(f.item.Text) + 1 // +1 换行
		if len(groups[f.kind]) == 0 {
			need += estimateTokens(recallHeader(f.kind))
		}
		if used+need > maxTokens {
			break
		}
		used += need
		groups[f.kind] = append(groups[f.kind], f.item.Text)
		seen[f.item.Key] = struct{}{}
		taint = types.PropagateTaint(taint, f.item.Taint)
	}
	for _, it := range profile {
		if it.Key == "" || containedInHistory(it.Key, history) {
			continue
		}
		if _, dup := seen[it.Key]; dup {
			continue
		}
		need := estimateTokens(it.Text) + 1
		if len(groups[fsm.RecallProfile]) == 0 {
			need += estimateTokens(recallHeader(fsm.RecallProfile))
		}
		if used+need > maxTokens {
			break
		}
		used += need
		groups[fsm.RecallProfile] = append(groups[fsm.RecallProfile], it.Text)
		seen[it.Key] = struct{}{}
		taint = types.PropagateTaint(taint, it.Taint)
	}
	var out strings.Builder
	for _, kind := range kinds {
		lines := groups[kind]
		if len(lines) == 0 {
			continue
		}
		out.WriteString(recallHeader(kind))
		out.WriteString(strings.Join(lines, "\n"))
		out.WriteByte('\n')
	}
	return out.String(), taint
}
