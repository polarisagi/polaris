package graphrag

import (
	"context"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/llm/safecall"
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
	docText := p.fetchDocText(ctx, docID)

	entities, err := p.entityExtractor.Extract(ctx, docText)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "GraphBuildPipeline: Phase1 entity extraction failed", err)
	}
	if len(entities) == 0 {
		return nil
	}

	for _, e := range entities {
		if e.SourceDocID == "" {
			e.SourceDocID = docID
		}
	}
	edges, inferred, err := p.relationExtractor.Extract(ctx, entities, docText)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "GraphBuildPipeline: Phase2 relation extraction failed", err)
	}
	if err := p.persistGraph(ctx, docID, entities, edges, inferred); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "GraphBuildPipeline: persist entities/relations failed", err)
	}

	// Phase 3 CrossDocumentLinking 由 persistGraph 落库时的 semantic_entities
	// UNIQUE(entity_type, name) 完成：不同文档抽出的同名同类实体归并为同一行，
	// 关系边挂在同一 DBID 上即跨文档链接。原内存 CrossDocumentLinker 只把实体
	// 自身 ID 追加进进程级 map、全仓无读取方，且无锁、随进程寿命无限增长
	// （GR-7.2-007），已删除。

	clusters := p.groupClusters(entities)

	// Phase 5: ConceptSynthesizer
	if err := p.synthesizeConcepts(ctx, entities, clusters); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "GraphBuildPipeline.Run", err)
	}

	return nil
}

// ExtractEntitiesAndRelations 执行 Phase1（实体抽取）+ Phase2（关系抽取），
// 不执行 Phase3-5（跨文档链接/聚类/概念合成——这些阶段面向长期积累的文档语料，
// 单次会话文本没有意义）。
//
// D3（原 GD-13-003，2026-07-25 推翻 ADR-0077 关于"不做管线合并"的结论，见
// ADR-0077）：本方法是 internal/memory/consolidation 与 RAG 文档摄取共享的
// 唯一 LLM 实体/关系抽取实现——不再各自维护独立 Prompt/LLM 调用，消除重复
// Token 燃烧与因 Prompt 差异导致的实体漂移。ADR-0077 的写入期去重桥接
// （GraphWriter.UpsertEntity 查重）与检索期联合种子机制不受影响、继续生效，
// 本次只统一"抽取"这一步，不改变两条管线各自的写入 API 与 Tier0/Tier1+
// 存储选型（consolidation 侧写入逻辑见 consolidation_extract.go upsertSemantic，
// 仍走 SQLite semantic_entities，未强制依赖 SurrealDB，兼容 Tier-0）。
func (p *GraphBuildPipeline) ExtractEntitiesAndRelations(ctx context.Context, sourceID, text string) ([]*Entity, []*Relation, error) {
	entities, err := p.entityExtractor.Extract(ctx, text)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInternal, "GraphBuildPipeline.ExtractEntitiesAndRelations: entity extraction failed", err)
	}
	if len(entities) == 0 {
		return nil, nil, nil
	}
	for _, e := range entities {
		if e.SourceDocID == "" {
			e.SourceDocID = sourceID
		}
	}

	edges, _, err := p.relationExtractor.Extract(ctx, entities, text)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInternal, "GraphBuildPipeline.ExtractEntitiesAndRelations: relation extraction failed", err)
	}
	return entities, edges, nil
}

func (p *GraphBuildPipeline) synthesizeConcepts(ctx context.Context, entities []*Entity, clusters map[int][]int) error { //nolint:gocyclo,nestif
	for _, cluster := range clusters {
		if err := p.synthesizeOneCluster(ctx, entities, cluster); err != nil {
			return err
		}
	}
	return nil
}

func (p *GraphBuildPipeline) synthesizeOneCluster(ctx context.Context, entities []*Entity, cluster []int) error { //nolint:nestif,gocyclo
	if len(cluster) < 3 {
		return nil // Only synthesize concepts for clusters with >= 3 entities
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
			resp, err := safecall.Infer(inferCtx, providerClient.provider, conceptMsgs)
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
		return nil // skip relations if concept entity resolution failed
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
	return nil
}

// fetchDocText 获取文档文本（fetcher 注入时从 store 取；否则降级用 docID 占位）。
func (p *GraphBuildPipeline) fetchDocText(ctx context.Context, docID string) string {
	if p.fetcher != nil {
		if text, err := p.fetcher.FetchText(ctx, docID); err == nil && text != "" {
			return text
		}
	}
	return docID
}

// groupClusters 聚类并按簇归组实体下标。clusterAssignments 的下标对应 embs（只含
// 有向量的实体），必须经 embIdx 映射回 entities 下标；原实现直接当作 entities
// 下标使用，任一实体缺向量时其后所有实体的簇归属整体错位。
func (p *GraphBuildPipeline) groupClusters(entities []*Entity) map[int][]int {
	embs, embIdx := collectEmbeddings(entities)
	clusters := make(map[int][]int)
	for i, cID := range p.clusterer.ClusterEntities(embs) {
		if cID == -1 || i >= len(embIdx) {
			continue // Skip noise/unclassified
		}
		clusters[cID] = append(clusters[cID], embIdx[i])
	}
	return clusters
}

// collectEmbeddings 返回有向量实体的向量列表及其在 entities 中的下标。
func collectEmbeddings(entities []*Entity) ([][]float32, []int) {
	embs := make([][]float32, 0, len(entities))
	idx := make([]int, 0, len(entities))
	for i, e := range entities {
		if len(e.Embedding) > 0 {
			embs = append(embs, e.Embedding)
			idx = append(idx, i)
		}
	}
	return embs, idx
}

// persistGraph 把 Phase1/2 产物写入语义图（GR-7.2-003 连带发现）。
//
// 原 Run 只在 Phase5 写入概念实体，文档抽取出的实体与关系从未落库——
// Phase5 为概念建边时按 (type,name) 反查源实体 DBID 也因此恒失败，整条
// 文档→知识图谱管线实际不产生任何图数据。写入走 SemanticMemory（与 Phase5、
// M5 consolidation 同一落库入口），不使用无生产接线的 GraphWriter。
//
// 已存在的同名同类型实体不重写：UpsertFact 的 ON CONFLICT 会用本次（空）
// properties 覆盖原值，文档摄取不应抹掉记忆侧已积累的实体属性，这里只需其 DBID。
// 外部文档内容至少按 TaintMedium 入库。inferred（共现回退）关系不落库。
func (p *GraphBuildPipeline) persistGraph(ctx context.Context, docID string, entities []*Entity, edges []*Relation, inferred bool) error {
	if p.semanticMem == nil {
		return nil
	}
	dbIDs, byID, err := p.persistEntities(ctx, entities)
	if err != nil || inferred {
		return err
	}
	return p.persistRelations(ctx, docID, edges, byID, dbIDs)
}

// docTaint 外部文档内容至少按 TaintMedium 入库。
func docTaint(levels ...types.TaintLevel) types.TaintLevel {
	t := types.TaintMedium
	for _, l := range levels {
		if l > t {
			t = l
		}
	}
	return t
}

// persistEntities 写入尚不存在的实体并返回 entity.ID → DBID 映射。
func (p *GraphBuildPipeline) persistEntities(ctx context.Context, entities []*Entity) (map[string]int64, map[string]*Entity, error) {
	dbIDs := make(map[string]int64, len(entities))
	byID := make(map[string]*Entity, len(entities))
	for _, e := range entities {
		if e.Name == "" || e.Type == "" {
			continue
		}
		byID[e.ID] = e
		existing, err := p.semanticMem.GetEntity(ctx, e.Type, e.Name)
		if err != nil || existing == nil {
			if err := p.semanticMem.UpsertFact(ctx, *e, docTaint(e.TaintLevel)); err != nil {
				return nil, nil, apperr.Wrap(apperr.CodeInternal, "persistGraph: upsert entity", err)
			}
			existing, err = p.semanticMem.GetEntity(ctx, e.Type, e.Name)
			if err != nil || existing == nil {
				continue
			}
		}
		dbIDs[e.ID] = existing.DBID
	}
	return dbIDs, byID, nil
}

// persistRelations 写入端点均已落库的关系；端点缺失（LLM 幻觉/拼写偏差）不建悬空边。
func (p *GraphBuildPipeline) persistRelations(ctx context.Context, docID string, edges []*Relation, byID map[string]*Entity, dbIDs map[string]int64) error {
	for _, r := range edges {
		from, to := byID[r.FromEntityID], byID[r.ToEntityID]
		if from == nil || to == nil || dbIDs[from.ID] == 0 || dbIDs[to.ID] == 0 {
			continue
		}
		taint := docTaint(from.TaintLevel, to.TaintLevel)
		rel := *r
		rel.FromDBID, rel.ToDBID = dbIDs[from.ID], dbIDs[to.ID]
		rel.SourceDocID = docID
		rel.TaintLevel = taint
		if rel.Weight == 0 {
			rel.Weight = 1.0
		}
		if err := p.semanticMem.UpsertRelation(ctx, rel, taint); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "persistGraph: upsert relation", err)
		}
	}
	return nil
}

type Entity = types.Entity

type Relation = types.Relation

// EntityFetcher 提供按名称获取现有实体以便进行消歧的接口。
type EntityFetcher interface {
	GetEntityByName(ctx context.Context, name string) (*Entity, error)
}
