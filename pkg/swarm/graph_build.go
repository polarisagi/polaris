package swarm

import (
	"context"
	"fmt"
	"strings"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
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
}

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

// Run 执行完整 5 阶段构建管线。
// Phase 1: EntityExtraction → Phase 2: RelationExtraction →
// Phase 3: CrossDocumentLinking → Phase 4: Clustering →
// Phase 5: ConceptSynthesizer.
func (p *GraphBuildPipeline) Run(ctx context.Context, docID string) error {
	// 获取文档文本（fetcher 注入时从 store 取；否则降级用 docID 占位）
	docText := docID
	if p.fetcher != nil {
		if text, err := p.fetcher.FetchText(ctx, docID); err == nil && text != "" {
			docText = text
		}
	}

	entities, err := p.entityExtractor.Extract(ctx, docText)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "GraphBuildPipeline: Phase1 entity extraction failed", err)
	}
	if len(entities) == 0 {
		return nil
	}

	edges, err := p.relationExtractor.Extract(ctx, entities)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "GraphBuildPipeline: Phase2 relation extraction failed", err)
	}

	if err := p.crossDocLinker.Link(ctx, entities, edges); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "GraphBuildPipeline: Phase3 cross-doc linking failed", err)
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
		return err
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
			var entityNames []string
			for _, idx := range cluster {
				entityNames = append(entityNames, entities[idx].Name)
			}
			prompt := fmt.Sprintf("请为以下实体列表提炼一个简短的概念标签（只输出标签内容，不要有其他解释）：\n%s", strings.Join(entityNames, ", "))
			if providerClient, ok := p.entityExtractor.llmClient.(*ProviderLLMClient); ok {
				resp, err := providerClient.provider.Infer(ctx, []protocol.Message{
					{Role: "user", Content: prompt},
				})
				if err == nil && resp != nil && resp.Content != "" {
					conceptLabel = strings.Split(strings.TrimSpace(resp.Content), "\n")[0]
				} else {
					errLLM = perrors.New(perrors.CodeInternal, "llm inference failed")
				}
			} else {
				errLLM = perrors.New(perrors.CodeInternal, "unsupported llm client type")
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

		sourceEntityIDs := make([]string, 0, len(cluster))
		for _, idx := range cluster {
			sourceEntityIDs = append(sourceEntityIDs, entities[idx].ID)
		}

		conceptEntity := protocol.Entity{
			ID:         "concept:" + conceptLabel,
			Name:       conceptLabel,
			Type:       "Concept",
			Properties: map[string]any{"cluster_size": len(cluster), "source_entities": sourceEntityIDs},
			// Assume ConceptSynthesizer runs in a context where event tracking is not direct, or trace to doc processing event
			TaintLevel: protocol.TaintLevel(0), // Inherit appropriately in real impl
		}

		if err := p.semanticMem.UpsertFact(ctx, conceptEntity); err != nil {
			return perrors.Wrap(perrors.CodeInternal, "GraphBuildPipeline: Phase5 upsert fact failed", err)
		}

		for _, idx := range cluster {
			rel := protocol.Relation{
				FromEntityID: entities[idx].ID,
				ToEntityID:   conceptEntity.ID,
				RelationType: "RELATED_TO",
				Weight:       1.0,
			}
			if err := p.semanticMem.UpsertRelation(ctx, rel); err != nil {
				return perrors.Wrap(perrors.CodeInternal, "GraphBuildPipeline: Phase5 upsert relation failed", err)
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

type Entity = protocol.Entity

type Relation = protocol.Relation

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
