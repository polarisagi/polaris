package observability

import "sync"

// cardinalityGuard LRU 基数守护（inv_M3_06: cap=500）。
// 超过 cap 后，新值映射为 "<overflow>" 桶，防止高基标签爆炸 Prometheus 内存。
type cardinalityGuard struct {
	mu    sync.Mutex
	index map[string]struct{}
	order []string
	cap   int
}

func newCardinalityGuard(cap int) *cardinalityGuard {
	return &cardinalityGuard{
		index: make(map[string]struct{}, cap),
		order: make([]string, 0, cap),
		cap:   cap,
	}
}

// Allow 若 value 已在 LRU 中则直接返回；否则加入，满时淘汰最旧条目（真 LRU）。
// 保证 Prometheus label 基数不超过 cap，同时不会因填满而永久丢弃新值。
func (g *cardinalityGuard) Allow(value string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.index[value]; ok {
		return value
	}
	if len(g.order) >= g.cap {
		evict := g.order[0]
		g.order = g.order[1:]
		delete(g.index, evict)
	}
	g.index[value] = struct{}{}
	g.order = append(g.order, value)
	return value
}

// 全局守护实例（cap=500，与 M03 §2.1 一致）。
// 豁免 R1.3：与 GlobalSurpriseIndex 同类——全程序生命周期、内部 mutex 守护、非业务状态。
var globalCardinalityGuard = newCardinalityGuard(500)

// toolCategory 将原始 tool name 映射为受控 label 值（inv_M3_06）。
// 映射规则：前缀 "mcp:" 或 "mcp_" → "mcp"；"skill:" 或 "sk_" → "skill"；其余 → "builtin"。
func toolCategory(name string) string {
	switch {
	case len(name) >= 4 && (name[:4] == "mcp:" || name[:4] == "mcp_"):
		return "mcp"
	case len(name) >= 6 && name[:6] == "skill:":
		return "skill"
	case len(name) >= 3 && name[:3] == "sk_":
		return "skill"
	default:
		return "builtin"
	}
}
