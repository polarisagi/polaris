package swarm

import (
	"context"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// Phase 1 — EntityExtraction

// EntityExtractor 实体提取器。
//
// 处理流程:
//  1. PreExtractionFilter (Tier 0 轻量, Go-native):
//     (a) 实体词典精确/模糊匹配 → 命中率>80% 标记 low_priority
//     (b) TF-IDF 关键词 → 与 entity_tfidf_centroid 余弦距离 → >noveltyThreshold 入 LLM 队列
//     (c) 禁止跨维度: TF-IDF（词汇维度）与 Embedding（潜空间维度）不可通约
//  2. LLM 提取: 过预筛 chunk → LLM 提取实体 (~200 tokens/call, Budget Pool 门控)
//  3. 自适应并发: cap=5, 间隔≥200ms; HTTP 429 → cap 减半 (floor=1)
//  4. 惰性提取: 未过预筛 chunk 在首次 BFS 命中时按需提取
//  5. 写入 entities 表: EntityID, Name, Type, Embedding, SourceDocID, SourceChunkID,
//     OccurrenceCount, TaintLevel(INT 0-4), SyncVersion(int64 LWW)
type EntityExtractor struct {
	dictMatcher    *EntityDictMatcher
	tfidfFilter    *TFIDFFilter
	llmClient      LLMClient
	concurrencyCap int // 5, 自适应调整
}

// PreExtractionFilter 预筛选: 实体词典 + TF-IDF。
func (ee *EntityExtractor) PreExtractionFilter(chunk string) (bool, float64) {
	if hitRate := ee.dictMatcher.Match(chunk); hitRate > 0.8 {
		return false, 0
	}
	novelty := ee.tfidfFilter.NoveltyScore(chunk)
	return novelty > 0.3, novelty
}

// Extract 从文档文本中提取实体列表。
// docText 是已加载的文档原始内容（非 docID），由调用方从 Store 取出后传入。
// 主路径: LLM 提取（DeepSeek ¥1/1M tokens，成本可忽略）。
// 回退: 规则引擎（词典匹配 + 正则模式）。
func (ee *EntityExtractor) Extract(ctx context.Context, docText string) ([]*Entity, error) {
	if ee.llmClient != nil {
		entities, err := ee.llmClient.ExtractEntities(ctx, docText)
		if err == nil && len(entities) > 0 {
			return entities, nil
		}
	}

	known := ee.dictMatcher.GetKnownEntities()
	if len(known) > 0 {
		return known, nil
	}

	return extractEntitiesByPattern(docText), nil
}

// entityPatterns 实体提取正则模式。
var entityPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b([A-Z][a-z]+(?:[A-Z][a-z]+)+)\b`),   // PascalCase
	regexp.MustCompile(`\b(?:[\w-]+/)+[\w.-]+\b`),             // 文件路径
	regexp.MustCompile(`\b(\d+\.\d+\.\d+)\b`),                 // 版本号
	regexp.MustCompile(`\b(?:https?://)?([\w-]+\.)+[\w-]+\b`), // 域名
	regexp.MustCompile(`\b(?:[A-Z][a-z]+){2,}\b`),             // 多词专有名词
}

var (
	versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	domainPattern  = regexp.MustCompile(`^[\w-]+\.[\w-]+`)
	pascalPattern  = regexp.MustCompile(`^[A-Z][a-z]+(?:[A-Z][a-z]+)+$`)
)

func extractEntitiesByPattern(text string) []*Entity {
	seen := make(map[string]bool)
	var entities []*Entity
	for _, pat := range entityPatterns {
		matches := pat.FindAllString(text, -1)
		for _, m := range matches {
			m = strings.TrimSpace(m)
			if len(m) < 3 || seen[m] {
				continue
			}
			seen[m] = true
			entities = append(entities, &Entity{
				ID:   m,
				Name: m,
				Type: classifyEntityType(m),
			})
		}
	}
	return entities
}

func classifyEntityType(name string) string {
	if strings.Contains(name, "/") {
		return "path"
	}
	if versionPattern.MatchString(name) {
		return "version"
	}
	if domainPattern.MatchString(name) {
		return "domain"
	}
	if strings.ContainsAny(name, "._") || pascalPattern.MatchString(name) {
		return "identifier"
	}
	return "concept"
}

// ---------------------------------------------------------------------------
// EntityDictMatcher 实体词典匹配器。

type EntityDictMatcher struct {
	exactMap map[string]*Entity
	fuzzyMap map[string][]*Entity
}

// Match 词典匹配——返回命中率 (0.0-1.0)。
func (em *EntityDictMatcher) Match(chunk string) float64 {
	if em.exactMap == nil && em.fuzzyMap == nil {
		return 0
	}
	words := tokenize(chunk)
	if len(words) == 0 {
		return 0
	}
	matched := 0
	for _, w := range words {
		if _, ok := em.exactMap[w]; ok {
			matched++
			continue
		}
		for key := range em.fuzzyMap {
			if editDistance(w, key) <= 2 {
				matched++
				break
			}
		}
	}
	return float64(matched) / float64(len(words))
}

func tokenize(text string) []string {
	parts := strings.Fields(text)
	var words []string
	for _, p := range parts {
		p = strings.Trim(p, ".,;:!?()[]{}\"'")
		if len(p) > 2 {
			words = append(words, strings.ToLower(p))
		}
	}
	return words
}

func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	n, m := len(ar), len(br)
	if n == 0 {
		return m
	}
	if m == 0 {
		return n
	}
	prev := make([]int, m+1)
	cur := make([]int, m+1)
	for j := 0; j <= m; j++ {
		prev[j] = j
	}
	for i := 1; i <= n; i++ {
		cur[0] = i
		for j := 1; j <= m; j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = minInt(prev[j]+1, minInt(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[m]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (em *EntityDictMatcher) GetKnownEntities() []*Entity {
	entities := make([]*Entity, 0, len(em.exactMap))
	for _, e := range em.exactMap {
		entities = append(entities, e)
	}
	return entities
}

// ---------------------------------------------------------------------------
// TFIDFFilter TF-IDF 预筛选器。
// 禁止跨维度: TF-IDF（词汇维度）与 Embedding（潜空间维度）不可通约。

type TFIDFFilter struct {
	idfWeights map[string]float64
	centroid   []float32 // entity_tfidf_centroid，同在稀疏词汇空间
}

// NoveltyScore 计算 chunk 与 entity_tfidf_centroid 的余弦距离。
// 无 IDF 词典数据时返回 0.5（中性值）。
func (tf *TFIDFFilter) NoveltyScore(chunk string) float64 {
	words := tokenize(chunk)
	if len(words) == 0 || len(tf.idfWeights) == 0 {
		return 0.5
	}
	tfidfVec := tf.computeTFIDF(words)
	sim := CosineSimilarity(tfidfVec, tf.centroid)
	return 1.0 - sim
}

func (tf *TFIDFFilter) computeTFIDF(words []string) []float32 {
	termFreq := make(map[string]int)
	for _, w := range words {
		termFreq[w]++
	}
	vec := make([]float32, len(tf.centroid))
	total := float64(len(words))
	idx := 0
	for term, idf := range tf.idfWeights {
		tfidf := (float64(termFreq[term]) / total) * idf
		if idx < len(vec) {
			vec[idx] = float32(tfidf)
		}
		idx++
	}
	return vec
}

// ---------------------------------------------------------------------------
// Phase 2 — RelationExtraction

// RelationExtractor 关系提取器。
// 文档内相邻段落实体共现 → LLM 判断关系类型 + description (~50 tokens).
type RelationExtractor struct {
	llmClient LLMClient
}

// Extract 从实体列表中提取关系边。
// 主路径: LLM 关系提取。回退: 同文档实体共现 → uses 关系。
func (re *RelationExtractor) Extract(ctx context.Context, entities []*Entity) ([]*Relation, error) {
	if re.llmClient != nil && len(entities) > 0 {
		docText := entities[0].SourceDocID
		relations, err := re.llmClient.ExtractRelations(ctx, entities, docText)
		if err == nil && len(relations) > 0 {
			return relations, nil
		}
	}

	var rels []*Relation
	for i := 0; i < len(entities); i++ {
		for j := i + 1; j < len(entities); j++ {
			rels = append(rels, &Relation{
				FromEntityID: entities[i].ID,
				ToEntityID:   entities[j].ID,
				RelationType: "uses",
			})
		}
	}
	return rels, nil
}
