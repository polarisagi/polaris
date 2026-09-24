package agentctx

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// recallSpec 一次记忆召回的全部输入，只含调用前从 sCtx 捕获的值：recall 跑在独立
// goroutine 里，预算到期被放弃后可能仍在运行，绝不能再读写 sCtx（否则与主路径数据竞争）。
type recallSpec struct {
	episodic         bool
	episodicQuery    string
	episodicK        int
	episodicHeader   string
	goal             string // 反思 / L2 语义 / RAG 的查询词；空则三者跳过
	reflectionHeader string
	withProfile      bool
	projectID        string
	knowledge        fsm.KnowledgeSearcher
}

// degradeOnRecallTimeout 召回超时按"无召回"降级（召回是增益，不是阶段的前置条件）；
// 其他错误照旧上抛，由 fsm 回退到无记忆 prompt。
func degradeOnRecallTimeout(phase string, err error) error {
	if err == nil {
		return nil
	}
	if apperr.IsCode(err, apperr.CodeTimeout) {
		slog.Warn("agentctx: memory recall exceeded budget, continuing without recalled memory", "phase", phase, "err", err)
		return nil
	}
	return apperr.Wrap(apperr.CodeInternal, phase, err)
}

// recallWithin 在 ctx 截止前等待召回完成，到期返回 CodeTimeout 由调用方降级。
//
// 只给下游传带截止的 ctx 不够：search.Embedder 接口不收 ctx，SyncBatcherAdapter
// 内部固定 Background+30s，截止时间传不下去（2026-09-25 实测 Perceive 因此卡 30s）。
// 故在边界处放弃等待；后台 goroutine 受下游 30s 上限约束必然退出（A-13）。
func recallWithin(ctx context.Context, memory protocol.MemoryFacade, cognitive fsm.CognitiveSearcher, spec recallSpec) (string, error) {
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	concurrent.SafeGo(ctx, "agentctx.recall", func(gctx context.Context) {
		text, err := recall(gctx, memory, cognitive, spec)
		done <- result{text, err}
	})
	select {
	case r := <-done:
		return r.text, r.err
	case <-ctx.Done():
		return "", apperr.Wrap(apperr.CodeTimeout, "agentctx: memory recall exceeded budget", ctx.Err())
	}
}

func recall(ctx context.Context, memory protocol.MemoryFacade, cognitive fsm.CognitiveSearcher, spec recallSpec) (string, error) {
	var out strings.Builder
	if spec.episodic {
		if err := writeEpisodic(ctx, &out, memory, spec); err != nil {
			return "", err
		}
	}
	if spec.goal != "" {
		writeReflections(ctx, &out, memory, spec)
	}
	if spec.withProfile {
		writeUserProfile(ctx, &out, memory)
	}
	if cognitive != nil && spec.goal != "" {
		writeSemantic(ctx, &out, memory, cognitive, spec)
	}
	if spec.knowledge != nil && spec.goal != "" {
		writeKnowledge(ctx, &out, spec)
	}
	return out.String(), nil
}

func writeEpisodic(ctx context.Context, out *strings.Builder, memory protocol.MemoryFacade, spec recallSpec) error {
	events, err := memory.ListEpisodicEvents(ctx, types.EpisodicQuery{
		Semantic:      spec.episodicQuery,
		ProjectID:     spec.projectID, // 情景记忆按项目隔离（ADR-0097 决策三修订）
		K:             spec.episodicK,
		MaxTaintLevel: types.TaintHigh,
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to query episodic memory", err)
	}
	if len(events) == 0 {
		return nil
	}
	out.WriteString(spec.episodicHeader)
	for _, e := range events {
		if pbEv := e.EventPtr(); pbEv != nil {
			fmt.Fprintf(out, "- [%s] %s: %s\n", pbEv.CreatedAt.Format(time.RFC3339), pbEv.Type, string(pbEv.Payload))
		}
	}
	return nil
}

func writeReflections(ctx context.Context, out *strings.Builder, memory protocol.MemoryFacade, spec recallSpec) {
	reflections, err := memory.ListReflections(ctx, types.ReflectionQuery{Topic: spec.goal, K: 3})
	if err != nil || len(reflections) == 0 {
		return
	}
	out.WriteString(spec.reflectionHeader)
	for _, r := range reflections {
		fmt.Fprintf(out, "- [%s] %s: %s\n", r.CreatedAt.Format(time.RFC3339), r.Strategy, r.Decision)
	}
}

func writeSemantic(ctx context.Context, out *strings.Builder, memory protocol.MemoryFacade, cognitive fsm.CognitiveSearcher, spec recallSpec) {
	hits, err := projectScopedFTS(ctx, memory, cognitive, spec.goal, 5, spec.projectID)
	if err != nil || len(hits) == 0 {
		return
	}
	out.WriteString("Semantic Memory (L2):\n")
	for _, r := range hits {
		fmt.Fprintf(out, "- [score=%.2f] %s\n", r.Score, r.Snippet)
	}
}

func writeKnowledge(ctx context.Context, out *strings.Builder, spec recallSpec) {
	hits, err := spec.knowledge.SearchRAG(ctx, spec.goal, 3)
	if err != nil || len(hits) == 0 {
		return
	}
	out.WriteString("Knowledge Base (RAG):\n")
	for _, r := range hits {
		fmt.Fprintf(out, "- [score=%.2f] %s: %s\n", r.Score, r.Source, r.Content)
	}
}

// writeUserProfile 消费 default 用户画像（P0-2）。
func writeUserProfile(ctx context.Context, out *strings.Builder, memory protocol.MemoryFacade) {
	p, err := memory.GetUserProfile(ctx, "default")
	if err != nil || p == nil {
		return
	}
	var summary []string
	for _, sf := range p.StableFacts {
		summary = append(summary, "- "+fmt.Sprint(sf))
	}
	for _, bp := range p.BehavioralPatterns {
		summary = append(summary, "- "+fmt.Sprint(bp))
	}
	if len(summary) > 0 {
		out.WriteString("## User Profile (Context)\n" + strings.Join(summary, "\n") + "\n")
	}
}
