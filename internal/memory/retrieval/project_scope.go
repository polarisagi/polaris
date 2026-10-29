package retrieval

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// 情景记忆项目作用域（ADR-0097 决策三修订，读取面 P6）
//
// 为什么包装整个 DocumentSource，而不是在七路召回里各加一行过滤：
// 七路分散在 source.go / retriever.go / retriever_helpers.go，逐路加过滤时漏一路即
// 泄漏，而漏的那一路在测试里不会红（ADR-0097 决策三原文）。包装层让"任何经
// DocumentSource 输出的片段都被过滤"成为结构事实——将来新增召回路也自动覆盖。
//
// 判定只依据 ScoredFragment.Source（各路统一的来源标识），按类型分三类：
//   - 情景事件（episodic:<id>，以及图路径直接以事件 ID 作 Source 的节点）→ 按事件归属过滤
//   - 持续簇（durative_group:<id>）→ 按簇归属过滤
//   - 其余（entity:/sement_/reflection:/chunk:）属用户全局层 → 放行
// ============================================================================

// isGlobalSource 确定属于用户全局层的来源（语义实体、反思、知识库）：不做 KV 反查，直接放行。
func isGlobalSource(src string) bool {
	return strings.HasPrefix(src, "entity:") || strings.HasPrefix(src, "sement_") ||
		strings.HasPrefix(src, "reflection:") || strings.HasPrefix(src, "chunk:")
}

type projectScopedSource struct {
	inner     search.ExtendedDocumentSource
	kv        protocol.Store
	projectID string

	mu    sync.Mutex
	cache map[string]bool // Source → 是否保留；HybridSearch 并发调用四个方法，需加锁
}

var _ search.ExtendedDocumentSource = (*projectScopedSource)(nil)

// newProjectScopedSource projectID 为空时不包装（后台/系统检索，不限项目）。
func newProjectScopedSource(inner search.ExtendedDocumentSource, kv protocol.Store, projectID string) search.ExtendedDocumentSource {
	if projectID == "" {
		return inner
	}
	return &projectScopedSource{inner: inner, kv: kv, projectID: projectID, cache: map[string]bool{}}
}

func (p *projectScopedSource) SearchBM25(ctx context.Context, query string, topK int) ([]types.ScoredFragment, error) {
	res, err := p.inner.SearchBM25(ctx, query, topK)
	return p.filter(ctx, res), err
}

func (p *projectScopedSource) SearchVector(ctx context.Context, embedding []float32, topK int) ([]types.ScoredFragment, error) {
	res, err := p.inner.SearchVector(ctx, embedding, topK)
	return p.filter(ctx, res), err
}

func (p *projectScopedSource) SearchGraph(ctx context.Context, query string, topK int) ([]types.ScoredFragment, error) {
	res, err := p.inner.SearchGraph(ctx, query, topK)
	return p.filter(ctx, res), err
}

func (p *projectScopedSource) SearchExtraPaths(ctx context.Context, query string, embedding []float32, topK int) ([]search.ExtraPath, error) {
	paths, err := p.inner.SearchExtraPaths(ctx, query, embedding, topK)
	for i := range paths {
		paths[i].Results = p.filter(ctx, paths[i].Results)
	}
	return paths, err //nolint:wrapcheck // 包装层只过滤结果、对错误透明：融合层按路降级并告警
}

func (p *projectScopedSource) filter(ctx context.Context, in []types.ScoredFragment) []types.ScoredFragment {
	if len(in) == 0 {
		return in
	}
	out := in[:0:0]
	for _, f := range in {
		if p.keep(ctx, f.Source) {
			out = append(out, f)
		}
	}
	return out
}

func (p *projectScopedSource) keep(ctx context.Context, src string) bool {
	p.mu.Lock()
	v, ok := p.cache[src]
	p.mu.Unlock()
	if ok {
		return v
	}
	v = p.decide(ctx, src)
	p.mu.Lock()
	p.cache[src] = v
	p.mu.Unlock()
	return v
}

func (p *projectScopedSource) decide(ctx context.Context, src string) bool {
	if isGlobalSource(src) {
		return true
	}
	switch {
	case strings.HasPrefix(src, "durative_group:"):
		return p.groupOwner(ctx, src) == p.projectID
	case strings.HasPrefix(src, "episodic_row:"):
		// 未回填 event_uuid 的历史投影行：无法反查归属，按默认项目处理（ADR 已知限制 1）。
		return p.projectID == types.DefaultProjectID
	case strings.HasPrefix(src, "episodic:"):
		owner, found := p.eventOwner(ctx, strings.TrimPrefix(src, "episodic:"))
		// 事件原文已不在 KV（被归档/删除）却仍被索引命中：无法判定归属，剔除。
		return found && owner == p.projectID
	}
	// 未知前缀：图路径把事件 ID 直接当 Source。能在情景层查到即按事件过滤，否则视为全局层。
	if owner, found := p.eventOwner(ctx, src); found {
		return owner == p.projectID
	}
	return true
}

func (p *projectScopedSource) eventOwner(ctx context.Context, id string) (string, bool) {
	raw, err := p.kv.Get(ctx, []byte("episodic:"+id))
	if err != nil || len(raw) == 0 {
		return "", false
	}
	var ev types.Event
	if json.Unmarshal(raw, &ev) != nil {
		return types.DefaultProjectID, true // 确属情景事件但解码失败：fail-closed 归默认项目
	}
	return ev.EffectiveProjectID(), true
}

func (p *projectScopedSource) groupOwner(ctx context.Context, src string) string {
	raw, err := p.kv.Get(ctx, []byte(src))
	if err != nil || len(raw) == 0 {
		return "" // 簇已不存在：不属于任何项目，剔除
	}
	var g store.DurativeGroup
	if json.Unmarshal(raw, &g) != nil {
		return ""
	}
	return g.EffectiveProjectID()
}
