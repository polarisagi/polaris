package tool

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/internal/ffi"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// semanticMatchThreshold 语义检索的最低余弦相似度，低于此值的候选不返回，
// 避免懒加载模式下 tool_search 把不相关工具也激活进会话（污染 tool_use 列表）。
const semanticMatchThreshold = 0.3

// toolSearchMaxResults 单次 tool_search 调用返回的工具数上限。
// 与 M13-bis-Extension-Registry.md §4 的 `LIMIT 10` 设计一致。
const toolSearchMaxResults = 10

// toolEmbeddingCacheMaxEntries 缓存条目上限（D-B6-02 修复：原实现无淘汰机制，
// 长期运行下动态注册/注销工具、MCP server 工具、LLM 生成技能会导致 byKey
// 无界增长，Tier-0 内存受限场景下构成缓慢内存泄漏）。超限后按插入顺序淘汰
// 最旧条目（FIFO，简化版 LRU，与 tool_helpers.go 的 lruCache 风格一致）。
const toolEmbeddingCacheMaxEntries = 1000

// toolEmbeddingCache 缓存工具描述的向量，避免每次 tool_search 调用都重新
// embed 全部候选（懒加载场景下工具总数可能上百个）。以工具名为 key，
// 描述文本变化极罕见（仅扩展升级时），不做基于内容变化的失效逻辑，最坏情况
// 下命中过期描述向量，语义相关性略有偏差但不影响正确性；条目数量本身通过
// FIFO 淘汰上限约束（防止无界增长）。
type toolEmbeddingCache struct {
	mu    sync.Mutex
	byKey map[string][]float32
	order []string // 插入顺序，用于超限时 FIFO 淘汰
}

func newToolEmbeddingCache() *toolEmbeddingCache {
	return &toolEmbeddingCache{byKey: make(map[string][]float32)}
}

func (c *toolEmbeddingCache) getOrEmbed(ctx context.Context, embedder search.Embedder, entry protocol.CatalogEntry) []float32 {
	c.mu.Lock()
	if v, ok := c.byKey[entry.Name]; ok {
		c.mu.Unlock()
		return v
	}
	c.mu.Unlock()

	text := entry.Name
	if entry.Description != "" {
		text = entry.Name + ": " + entry.Description
	}
	emb := embedder.Embed(ctx, text)

	c.mu.Lock()
	if _, exists := c.byKey[entry.Name]; !exists {
		c.order = append(c.order, entry.Name)
	}
	c.byKey[entry.Name] = emb
	for len(c.order) > toolEmbeddingCacheMaxEntries {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.byKey, oldest)
	}
	c.mu.Unlock()
	return emb
}

// scoredTool 是语义检索/子串匹配两路统一的候选结果，score 越大越相关。
type scoredTool struct {
	entry protocol.CatalogEntry
	score float32
}

// toolSearchMatcher 持有单次 tool_search 调用的匹配状态，把 MakeToolSearchFn
// 内部原本揉在一个闭包里的"语义检索 + 子串兜底 + 排序截断"拆成独立方法，
// 避免闭包圈复杂度超过 lint 阈值（gocyclo/nestif）。
type toolSearchMatcher struct {
	embedder search.Embedder
	embCache *toolEmbeddingCache
	byName   map[string]scoredTool
}

func newToolSearchMatcher(embedder search.Embedder, embCache *toolEmbeddingCache, capHint int) *toolSearchMatcher {
	return &toolSearchMatcher{embedder: embedder, embCache: embCache, byName: make(map[string]scoredTool, capHint)}
}

func (m *toolSearchMatcher) upsert(entry protocol.CatalogEntry, score float32) {
	if existing, ok := m.byName[entry.Name]; !ok || score > existing.score {
		m.byName[entry.Name] = scoredTool{entry: entry, score: score}
	}
}

// matchSemantic 用 query 与每个候选描述向量的余弦相似度做语义检索，
// 命中阈值（semanticMatchThreshold）以上的候选才会被采纳。
func (m *toolSearchMatcher) matchSemantic(ctx context.Context, all []protocol.CatalogEntry, query string) {
	if m.embedder == nil || query == "" {
		return
	}
	queryEmb := m.embedder.Embed(ctx, query)
	if len(queryEmb) == 0 {
		return
	}
	for _, t := range all {
		candidateEmb := m.embCache.getOrEmbed(ctx, m.embedder, t)
		if len(candidateEmb) == 0 {
			continue
		}
		if sim := ffi.VecCosineF32(queryEmb, candidateEmb); sim >= semanticMatchThreshold {
			m.upsert(t, sim)
		}
	}
}

// matchKeywords 关键词 / BM25 匹配（ADR-0119）：对工具名称（加权）、描述分词后打分，不需要 Embedder。
// query 为空（列出全部）时给所有候选固定低分；工具名被 query 整词包含时直接置顶。
// 分数归一到 (0,1]，与语义检索的余弦分在 upsert 里取大者。
func (m *toolSearchMatcher) matchKeywords(all []protocol.CatalogEntry, query string) {
	if query == "" {
		for _, t := range all {
			m.upsert(t, 0.5)
		}
		return
	}
	scores := bm25Scores(all, query)
	var maxScore float64
	for _, sc := range scores {
		maxScore = max(maxScore, sc)
	}
	if maxScore <= 0 {
		return
	}
	lowerQuery := strings.ToLower(query)
	for i, t := range all {
		if scores[i] <= 0 {
			continue
		}
		norm := float32(scores[i] / maxScore * 0.9)
		if len(t.Name) >= 3 && strings.Contains(lowerQuery, strings.ToLower(t.Name)) {
			norm = 1 // 点名匹配
		}
		m.upsert(t, norm)
	}
}

// sortedTop 按分数降序（同分按名称保证输出确定性）返回最多 limit 条结果。
func (m *toolSearchMatcher) sortedTop(limit int) []scoredTool {
	matches := make([]scoredTool, 0, len(m.byName))
	for _, s := range m.byName {
		matches = append(matches, s)
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].entry.Name < matches[j].entry.Name
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}

// sessionIDFromCtx 提取激活作用域 ID——以 TaskID 作为激活作用域：
// `internal/execute/dag/executor.go` 的 Execute() 已经把 taskID 通过
// protocol.CtxTaskIDKey 注入 ctx 并向下传递到每个节点的工具调用，是本仓库
// 唯一真正被生产路径注入的会话级标识（原先用裸字符串 "session_id" 查
// ctx.Value，但全仓库没有任何地方真正写入这个 key，导致 ActivateTool 从未
// 被真实调用——复用已接线的 TaskID 修复此问题）。
func sessionIDFromCtx(ctx context.Context) string {
	if tid, ok := ctx.Value(protocol.CtxTaskIDKey{}).(string); ok {
		return tid
	}
	return ""
}

type toolSearchSummary struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
}

// activateAndSummarize 把匹配结果激活到当前会话（若有）并转换为对外 JSON 结构。
func activateAndSummarize(compCatalog *catalog.CompositeCatalog, sessionID string, matches []scoredTool) []toolSearchSummary {
	results := make([]toolSearchSummary, 0, len(matches))
	for _, m := range matches {
		if sessionID != "" {
			compCatalog.ActivateTool(sessionID, m.entry.Name)
		}
		results = append(results, toolSearchSummary{
			Name:        m.entry.Name,
			Description: m.entry.Description,
			Source:      string(m.entry.Source),
		})
	}
	return results
}

// MakeToolSearchFn 让 Agent 在已注册工具集中按名称/描述关键词搜索，并把命中的工具激活到当前会话
// （基于 ctx 的 TaskID）；激活后的工具在**下一轮**请求的 tools 数组里出现。
//
// 匹配策略（ADR-0119）：关键词/BM25 是基线，任何机器可用；Embedder 可用时叠加语义检索
// （复用 M1 Embedder + internal/ffi VecCosineF32），两路结果合并去重后按分数降序，最多 toolSearchMaxResults 条。
// 懒加载模式下已常驻的核心工具不返回（它们已经可调用，返回只会占用结果名额）。
func MakeToolSearchFn(compCatalog *catalog.CompositeCatalog, embedder search.Embedder) sandbox.InProcessFn {
	embCache := newToolEmbeddingCache()

	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "tool_search: invalid args", err)
		}

		all := compCatalog.List(ctx, types.TrustUntrusted) // search across all
		if compCatalog.LazyActive(ctx, types.TrustUntrusted) {
			all = excludeCore(compCatalog, all)
		}
		query := strings.TrimSpace(args.Query)

		matcher := newToolSearchMatcher(embedder, embCache, len(all))
		matcher.matchSemantic(ctx, all, query)
		matcher.matchKeywords(all, query)

		matches := matcher.sortedTop(toolSearchMaxResults)
		results := activateAndSummarize(compCatalog, sessionIDFromCtx(ctx), matches)

		out := map[string]any{"tools": results, "total": len(results)}
		if len(results) > 0 {
			out["note"] = "已激活，下一轮可直接调用；参数以下一轮收到的工具定义为准。"
		} else {
			out["note"] = "没有匹配的工具。换用工具名或更具体的关键词再试一次。"
		}
		return json.Marshal(out)
	}
}

// excludeCore 去掉常驻核心工具（已在 tools 数组里，无需激活）。
func excludeCore(c *catalog.CompositeCatalog, all []protocol.CatalogEntry) []protocol.CatalogEntry {
	out := make([]protocol.CatalogEntry, 0, len(all))
	for _, e := range all {
		if !c.IsCore(e.Name) {
			out = append(out, e)
		}
	}
	return out
}
