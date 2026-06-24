package consolidation

import (
	"context"
	"log/slog"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// OnDemandExtractor 查询时对无实体关联的 episodic 事件执行即时实体提取。
// 触发条件：HybridRetriever 返回结果中，embed_model_version=” 且无对应 semantic_entities 的事件。
// 提取结果立即写入 SemanticMem（write-through cache 语义）。
//
// 注意：这是热路径的异步辅助，不阻塞查询返回。
type OnDemandExtractor struct {
	extractor *ConsolidationPipeline // 复用 Stage 1 llmExtract 逻辑
	episodic  protocol.EpisodicMemory
}

// NewOnDemandExtractor 创建即时提取器，pipeline 必须已注入 provider。
func NewOnDemandExtractor(pipeline *ConsolidationPipeline, episodic protocol.EpisodicMemory) *OnDemandExtractor {
	return &OnDemandExtractor{extractor: pipeline, episodic: episodic}
}

// ExtractAsync 对给定的 scored 事件列表，异步提取未关联实体的事件。
// 不阻塞调用方；提取完成后写入语义记忆。
func (oe *OnDemandExtractor) ExtractAsync(ctx context.Context, events []types.ScoredEvent) {
	if oe.extractor == nil || len(events) == 0 {
		return
	}

	// 筛选出尚未提取过实体的事件（启发式：embed_model_version 为空 = 从未处理）
	var unextracted []types.ScoredEvent
	for _, e := range events {
		if pbEv, _ := e.Event.(*types.Event); pbEv == nil || pbEv.EmbedModelVersion == "" {
			unextracted = append(unextracted, e)
		}
	}
	if len(unextracted) == 0 {
		return
	}

	// 异步执行，不阻塞查询响应
	concurrent.SafeGo(context.Background(), "on-demand-extractor", func(ctx context.Context) {
		// 使用独立超时，不继承查询 ctx（查询可能已结束）
		extractCtx, cancel := context.WithTimeout(ctx, consolidationTimeout)
		defer cancel()

		entities, relations, err := oe.extractor.extractEntitiesAndRelations(
			extractCtx,
			"on_demand",
			unextracted,
		)
		if err != nil || (len(entities) == 0 && len(relations) == 0) {
			return
		}

		maxTaint := types.TaintNone
		for _, ev := range unextracted {
			if pbEv, _ := ev.Event.(*types.Event); pbEv != nil {
				if pbEv.TaintLevel > maxTaint {
					maxTaint = pbEv.TaintLevel
				}
			}
		}
		if maxTaint < types.TaintMedium {
			maxTaint = types.TaintMedium
		}

		if err := oe.extractor.upsertSemantic(extractCtx, entities, relations, maxTaint); err != nil {
			slog.Warn("on_demand_extractor: upsert failed", "err", err)
		}
	})
}
