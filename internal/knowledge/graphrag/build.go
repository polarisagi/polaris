package graphrag

import (
	"context"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/observability/trace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// GraphBuildPipeline — 知识图谱构建管线（5 阶段）。
// 架构文档: docs/arch/M10-Knowledge-RAG.md §2.7

// DocFetcher 文档内容获取接口（consumer-side，防包循环）。
// 由调用方注入，返回指定 docID 的原始文本内容。
type DocFetcher interface {
	FetchText(ctx context.Context, docID string) (string, error)
}

type GraphBuildPipeline struct {
	entityExtractor   *EntityExtractor
	relationExtractor *RelationExtractor
	crossDocLinker    *CrossDocumentLinker
	clusterer         *Clusterer
	semanticMem       protocol.SemanticMemory
	fetcher           DocFetcher // optional：nil 时将 docID 本身作为文本占位
	gate              backgroundGate
}

type backgroundGate interface {
	BackgroundPermit(priority int) bool
}

func (p *GraphBuildPipeline) WithBackgroundGate(g backgroundGate) { p.gate = g }

// NewGraphBuildPipeline 构造知识图谱构建管线。
// llm 可为 nil（Tier 0 降级正则提取 + 共现关系推断）。
// tier 决定聚类策略：0=Mini-Batch K-Means，1+=DBSCAN。
func NewGraphBuildPipeline(llm LLMClient, tier int, semanticMem protocol.SemanticMemory) *GraphBuildPipeline {
	return &GraphBuildPipeline{
		entityExtractor: &EntityExtractor{
			dictMatcher:    &EntityDictMatcher{exactMap: make(map[string]*Entity), fuzzyMap: make(map[string][]*Entity)},
			tfidfFilter:    &TFIDFFilter{},
			llmClient:      llm,
			concurrencyCap: 5,
		},
		relationExtractor: &RelationExtractor{llmClient: llm},
		crossDocLinker:    &CrossDocumentLinker{linkedEntities: make(map[string][]string)},
		clusterer:         NewClusterer(tier),
		semanticMem:       semanticMem,
	}
}

// SetDocFetcher 注入文档内容获取器（可选；nil 时降级为规则提取）。
func (p *GraphBuildPipeline) SetDocFetcher(f DocFetcher) { p.fetcher = f }

// WithSummarizer 注入社区摘要生成器（可选；转发至内部 Clusterer，见 cluster.go
// WithSummarizer 注释——2026-07-08 恢复接线）。
func (p *GraphBuildPipeline) WithSummarizer(s *CommunityGenerativeSummarizer) {
	p.clusterer.WithSummarizer(s)
}

// Run 执行完整 5 阶段构建管线。
// Phase 1: EntityExtraction → Phase 2: RelationExtraction →
// Phase 3: CrossDocumentLinking → Phase 4: Clustering →
// Phase 5: ConceptSynthesizer.
func (p *GraphBuildPipeline) Run(ctx context.Context, docID string) error {
	if p.gate != nil && !p.gate.BackgroundPermit(3) {
		return nil
	}
	// 获取文档文本（fetcher 注入时从 store 取；否则降级用 docID 占位）
	docText := docID
	if p.fetcher != nil {
		if text, err := p.fetcher.FetchText(ctx, docID); err == nil && text != "" {
			docText = text
		}
	}

	entities, err := p.entityExtractor.Extract(ctx, docText)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "GraphBuildPipeline: Phase1 entity extraction failed", err)
	}
	if len(entities) == 0 {
		return nil
	}

	edges, err := p.relationExtractor.Extract(ctx, entities)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "GraphBuildPipeline: Phase2 relation extraction failed", err)
	}

	if err := p.crossDocLinker.Link(ctx, entities, edges); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "GraphBuildPipeline: Phase3 cross-doc linking failed", err)
	}

	clusterAssignments := p.clusterer.ClusterEntities(collectEmbeddings(entities))

	// Group entities by cluster ID
	clusters := make(map[int][]int)
	for idx, cID := range clusterAssignments {
		if cID == -1 {
			continue // Skip noise/unclassified
		}
		clusters[cID] = append(clusters[cID], idx)
	}

	// Phase 5: ConceptSynthesizer
	if err := p.synthesizeConcepts(ctx, entities, clusters); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "GraphBuildPipeline.Run", err)
	}

	return nil
}

func (p *GraphBuildPipeline) synthesizeConcepts(ctx context.Context, entities []*Entity, clusters map[int][]int) error { //nolint:gocyclo,nestif
	for _, cluster := range clusters {
		if len(cluster) < 3 {
			continue // Only synthesize concepts for clusters with >= 3 entities
		}

		var conceptLabel string
		var errLLM error
		if p.entityExtractor.llmClient != nil { //nolint:nestif
			var entityNames []string //nolint:prealloc
			for _, idx := range cluster {
				entityNames = append(entityNames, entities[idx].Name)
			}
			// A-12：System/User 消息分离，实体名列表（来自 DB）作为 User 消息，不拼入 System。
			if providerClient, ok := p.entityExtractor.llmClient.(*ProviderLLMClient); ok {
				conceptMsgs := []types.Message{
					{
						Role:    "system",
						Content: "你是知识图谱概念提炼助手。请为用户提供的实体列表提炼一个简短的概念标签，只输出标签内容，不要有其他解释。",
					},
					{
						Role:    "user",
						Content: strings.Join(entityNames, ", "),
					},
				}
				// P-1：每次 LLM 调用自持超时（90s，A-05）。
				inferCtx, inferCancel := context.WithTimeout(ctx, 90*time.Second)
				defer inferCancel()
				start := time.Now()
				//nolint:bare-infer // 历史代码暂留，后续重构替换
				resp, err := providerClient.provider.Infer(inferCtx, conceptMsgs)
				latencyMs := time.Since(start).Milliseconds()
				if err == nil && resp != nil && resp.Content != "" {
					conceptLabel = strings.Split(strings.TrimSpace(resp.Content), "\n")[0]
					trace.RecordLLMCall(ctx,
						"ProviderLLMClient",
						providerClient.model,
						"success",
						float64(latencyMs),
						resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.CacheHitTokens,
						0,
					)
				} else {
					trace.RecordLLMCall(ctx, "ProviderLLMClient", providerClient.model, "error", float64(latencyMs), 0, 0, 0, 0)
					if err != nil {
						errLLM = apperr.Wrap(apperr.CodeInternal, "llm inference failed", err)
					} else {
						errLLM = apperr.New(apperr.CodeInternal, "llm inference failed: empty response")
					}
				}
			} else {
				errLLM = apperr.New(apperr.CodeInternal, "unsupported llm client type")
			}
		}

		if p.entityExtractor.llmClient == nil || errLLM != nil {
			// Fallback: use highest occurrence entity name
			highestIdx := cluster[0]
			for _, idx := range cluster {
				if entities[idx].OccurrenceCount > entities[highestIdx].OccurrenceCount {
					highestIdx = idx
				}
			}
			conceptLabel = entities[highestIdx].Name
		}

		var maxTaint types.TaintLevel
		sourceEntityIDs := make([]string, 0, len(cluster))
		for _, idx := range cluster {
			sourceEntityIDs = append(sourceEntityIDs, entities[idx].ID)
			if entities[idx].TaintLevel > maxTaint {
				maxTaint = entities[idx].TaintLevel
			}
		}
		if maxTaint < types.TaintMedium {
			maxTaint = types.TaintMedium
		}

		conceptEntity := types.Entity{
			ID:         "concept:" + conceptLabel,
			Name:       conceptLabel,
			Type:       "Concept",
			Properties: map[string]any{"cluster_size": len(cluster), "source_entities": sourceEntityIDs},
			TaintLevel: maxTaint,
		}

		if err := p.semanticMem.UpsertFact(ctx, conceptEntity, maxTaint); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "GraphBuildPipeline: Phase5 upsert fact failed", err)
		}

		// fetch DBID for the concept entity we just created/updated
		conceptDBEntity, err := p.semanticMem.GetEntity(ctx, "Concept", conceptLabel)
		if err != nil || conceptDBEntity == nil {
			continue // skip relations if concept entity resolution failed
		}

		for _, idx := range cluster {
			// fetch DBID for the source entity
			srcEntity := entities[idx]
			srcDBEntity, err := p.semanticMem.GetEntity(ctx, srcEntity.Type, srcEntity.Name)
			if err != nil || srcDBEntity == nil {
				continue // skip relation if source entity resolution failed
			}

			rel := types.Relation{
				FromEntityID: srcEntity.ID,
				ToEntityID:   conceptEntity.ID,
				FromDBID:     srcDBEntity.DBID,     // MUST fill
				ToDBID:       conceptDBEntity.DBID, // MUST fill
				RelationType: "RELATED_TO",
				Weight:       1.0,
				TaintLevel:   maxTaint,
			}
			if err := p.semanticMem.UpsertRelation(ctx, rel, maxTaint); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "GraphBuildPipeline: Phase5 upsert relation failed", err)
			}
		}
	}
	return nil
}

func collectEmbeddings(entities []*Entity) [][]float32 {
	embs := make([][]float32, 0, len(entities))
	for _, e := range entities {
		if len(e.Embedding) > 0 {
			embs = append(embs, e.Embedding)
		}
	}
	return embs
}

type Entity = types.Entity

type Relation = types.Relation

// CrossDocumentLinker 跨文档实体链接。
// 新实体查同 Name+Type 已有实体 → CrossDocLink(EntityID, DocIDs[]).
type CrossDocumentLinker struct {
	linkedEntities map[string][]string // entityID → []docID
}

func (cdl *CrossDocumentLinker) Link(ctx context.Context, entities []*Entity, edges []*Relation) error {
	for _, e := range entities {
		cdl.linkedEntities[e.ID] = append(cdl.linkedEntities[e.ID], e.ID)
	}
	return nil
}

// EntityFetcher 提供按名称获取现有实体以便进行消歧的接口。
type EntityFetcher interface {
	GetEntityByName(ctx context.Context, name string) (*Entity, error)
}
