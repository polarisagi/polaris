// adapters_recall.go — 回合内召回 L2 语义来源的适配器（ADR-0105 决策十 WP8）。
package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/agent"
	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/types"
)

// 核实结论（2026-09-30，代码事实）：SurrealDB FTS 索引是共享索引，里面混存四类文档，且**只存倒排索引不存正文**：
//   - 情景事件（docID = Event.ID，EpisodicMem.Append / CognitiveReplayer 写入；向量索引同样只有它与 ext_）
//   - 语义实体（docID = "sement_"+type+"_"+name，SemanticMem.UpsertFact 写入；**只进 FTS，没有向量**）
//   - 反思洞察（docID = "her_"+taskID，reflexion 写入）
//   - 扩展目录（docID = "ext_"+extID，扩展库员/插件索引器写入，属工具发现而非记忆）
//
// 按"同一数据只经一条检索路径进入 prompt"分流（ADR-0105 决策十 WP8/WP11）：
//   - FTSSearch（fsm.CognitiveSearcher，L2 来源）只放行 "sement_" 语义实体，并按 ID 回取实体正文——这也是
//     Snippet 的唯一来源（FTS 命中本身没有正文，spec 09 RAG 清单"禁止用 ID 代替 Content"）。
//   - FTSEpisodic（fsm.EpisodicSearcher，情景来源）只返回情景事件候选 ID + BM25 分，不取正文；
//     归属项目、正文与污点由调用方经 MemoryFacade 判定（项目隔离）。
//   - 反思（her_）有反思来源、扩展目录（ext_）不是记忆：两条路径一律不放行。
// 向量路无实体数据，且情景向量查询侧要多一次 embedding，故适配器只实现 FTS 一路。

const (
	entityDocPrefix = "sement_"
	// entityFTSOverfetch 共享 FTS 按 BM25 混排，实体命中会被情景事件挤占名次：放大取回量再筛出实体。
	entityFTSOverfetch = 3
	// maxEntityIDSplits docID 是 type 与 name 用 "_" 直接拼接，二者都可能含下划线，逐个切点试取；
	// 有界以免畸形 ID 放大数据库查询。
	maxEntityIDSplits = 4
	// graphRAGSourceType 由 RAG 文档抽取的实体：正文已在 RAG 来源里（带来源 URI），这里再召一遍是重复计费。
	graphRAGSourceType = "graphrag_ingest"
)

// ftsSearcher 消费方私有接口（R1.4）：*store.SurrealDBCoreStore 满足。
type ftsSearcher interface {
	FTSSearch(query string, k int) ([]store.ScoredID, error)
}

// entityReader 消费方私有接口（R1.4）：protocol.SemanticMemory 的子集。
type entityReader interface {
	GetEntity(ctx context.Context, entityType, name string) (*types.Entity, error)
}

// isNonEpisodicDoc 非情景文档（实体/反思/扩展目录）的 docID：FTSEpisodic 据此预先剔除，省得调用方为它们
// 白做一次归属反查。这只是优化——是否真为情景事件仍由调用方经 EpisodicProjectOf 判定。
func isNonEpisodicDoc(id string) bool {
	return strings.HasPrefix(id, entityDocPrefix) || strings.HasPrefix(id, "her_") || strings.HasPrefix(id, "ext_")
}

var (
	_ fsm.CognitiveSearcher = (*recallCognitiveAdapter)(nil)
	_ fsm.EpisodicSearcher  = (*recallCognitiveAdapter)(nil)
)

// recallCognitiveAdapter 把 SurrealDB FTS + 语义实体库适配为 fsm.CognitiveSearcher / fsm.EpisodicSearcher。
type recallCognitiveAdapter struct {
	fts ftsSearcher
	sem entityReader
	now func() time.Time
}

func newRecallCognitiveAdapter(fts ftsSearcher, sem entityReader) *recallCognitiveAdapter {
	return &recallCognitiveAdapter{fts: fts, sem: sem, now: time.Now}
}

// FTSSearch 返回与 query 相关的活跃语义实体（污点随实体返回，由召回按 MaxTaint 过滤）。
// 非实体命中（情景/反思/扩展目录）一律丢弃——它们各有来源，且情景命中涉及项目隔离。
func (a *recallCognitiveAdapter) FTSSearch(ctx context.Context, query string, k int) ([]fsm.CogResult, error) {
	if a.fts == nil || a.sem == nil || k <= 0 {
		return nil, nil
	}
	hits, err := a.fts.FTSSearch(query, k*entityFTSOverfetch)
	if err != nil {
		return nil, err //nolint:wrapcheck // store 层已用 apperr 包装；召回按"无结果"降级
	}
	nowMs := a.now().UnixMilli()
	out := make([]fsm.CogResult, 0, k)
	for _, h := range hits {
		rest, ok := strings.CutPrefix(h.ID, entityDocPrefix)
		if !ok {
			continue
		}
		ent := a.lookup(ctx, rest)
		if ent == nil || !entityRecallable(ent, nowMs) {
			continue
		}
		out = append(out, fsm.CogResult{
			DocID:   h.ID,
			Snippet: entitySnippet(ent),
			Score:   float32(h.Score),
			Taint:   ent.TaintLevel,
		})
		if len(out) == k {
			break
		}
	}
	return out, nil
}

// FTSEpisodic 返回共享 FTS 里可能是情景事件的命中（DocID + BM25 分，保持 BM25 降序）。
// 失败原样上抛，由情景来源按"无结果"降级；fts 未注入（Tier0）时返回空。
func (a *recallCognitiveAdapter) FTSEpisodic(_ context.Context, query string, k int) ([]fsm.CogResult, error) {
	if a.fts == nil || k <= 0 {
		return nil, nil
	}
	hits, err := a.fts.FTSSearch(query, k)
	if err != nil {
		return nil, err //nolint:wrapcheck // store 层已用 apperr 包装；召回按"无结果"降级
	}
	out := make([]fsm.CogResult, 0, len(hits))
	for _, h := range hits {
		if isNonEpisodicDoc(h.ID) {
			continue
		}
		out = append(out, fsm.CogResult{DocID: h.ID, Score: float32(h.Score)})
	}
	return out, nil
}

// lookup 把 "type_name" 还原为实体：按下划线切点依次尝试，第一个存在的即是。
func (a *recallCognitiveAdapter) lookup(ctx context.Context, rest string) *types.Entity {
	tried := 0
	for i := 1; i < len(rest) && tried < maxEntityIDSplits; i++ {
		if rest[i] != '_' {
			continue
		}
		tried++
		if ent, err := a.sem.GetEntity(ctx, rest[:i], rest[i+1:]); err == nil && ent != nil {
			return ent
		}
	}
	return nil
}

// entityRecallable 实体必须仍然有效：FTS 索引不随实体状态变更而删除（supersede/expire 只改 SQLite 行）。
func entityRecallable(e *types.Entity, nowMs int64) bool {
	if e.Status != "" && e.Status != "active" {
		return false
	}
	if e.SourceType == graphRAGSourceType {
		return false
	}
	if e.ValidFrom > 0 && e.ValidFrom > nowMs {
		return false
	}
	if e.ValidUntil > 0 && e.ValidUntil <= nowMs {
		return false
	}
	return true
}

// entitySnippet 实体正文：有 description 用之，否则压成单行 JSON（map 序列化键有序，字节确定）。
// 生命周期/溯源元数据（source_type、valid_*）对模型无信息量，不输出。
func entitySnippet(e *types.Entity) string {
	desc, _ := e.Properties["description"].(string)
	if strings.TrimSpace(desc) != "" {
		return e.Name + ": " + desc
	}
	props := make(map[string]any, len(e.Properties))
	for k, v := range e.Properties {
		switch k {
		case "description", "source_type", "valid_from", "valid_until":
			continue
		}
		props[k] = v
	}
	if len(props) == 0 {
		return e.Name
	}
	b, err := json.Marshal(props)
	if err != nil {
		return e.Name
	}
	return e.Name + ": " + string(b)
}

// wireL2Recall 注入 L2 语义召回；从 buildAgent 抽出只为压低其圈复杂度，语义见调用处注释。
func wireL2Recall(a *agent.Agent, sb *SubstrateBundle, mb *MemoryBundle) {
	if sb.SurrealStore != nil {
		a.SetCognitiveSearcher(newRecallCognitiveAdapter(sb.SurrealStore, mb.Mem.Semantic()))
	}
}
