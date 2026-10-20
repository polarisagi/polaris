// Package pricing 是模型单价目录（USD / 百万 token），llm_calls 记账与历史迁移的单一事实源。
//
// 为什么独立成表：用户常把 DeepSeek 等模型接在通用 openai_compat 适配器下，适配器自身只知道
// OpenAI 的价格，按适配器类型取价会把 DeepSeek 流量按 gpt-4o-mini 计费。费用必须按"实际
// 调用的模型 ID"取价，所以记账时先查本表，查不到才回退适配器声明的 ProviderCapabilities。
package pricing

import (
	"strings"

	"github.com/polarisagi/polaris/pkg/types"
)

// Price 单位：USD / 百万 token。CacheHit<=0 表示无单独命中价（按 Input 计）。
type Price struct {
	Input    float64
	CacheHit float64
	Output   float64
}

// Entry 一条目录项。Prefix 按小写前缀匹配（取最长者），Source 为价格出处。
type Entry struct {
	Prefix string
	Price  Price
	// ContextWindow 上下文窗口（token），0 = 未知。状态栏的"上下文占用"以它为分母；
	// 优先用探测到的目录（provider_discovered_models），这里是目录缺失时的回退。
	ContextWindow int
	Source        string
}

// deepseekSource 官方定价页。页面注明高峰/低谷两档（低谷为高峰的一半，UTC 工作日
// 01:00–04:00 与 06:00–10:00 为高峰），这里取高峰价：估算偏高而不偏低，预算护栏更安全。
// 页面只写 "$" 符号、未点明币种，按 USD 存储。核实日期 2026-10-20。
const deepseekSource = "https://api-docs.deepseek.com/quick_start/pricing"

// entries 顺序无关，Lookup 取最长前缀。
//
//nolint:gochecknoglobals // 只读价目常量表
var entries = []Entry{
	{"deepseek-flash", Price{Input: 0.30, CacheHit: 0.006, Output: 1.20}, 1_000_000, deepseekSource},
	// 官方页说明 deepseek-v4-flash 仍可用且按 Flash 计价。
	{"deepseek-v4-flash", Price{Input: 0.30, CacheHit: 0.006, Output: 1.20}, 1_000_000, deepseekSource},
	{"deepseek-v4-pro", Price{Input: 1.32, CacheHit: 0.044, Output: 3.96}, 1_000_000, deepseekSource},
	{"gpt-4o-mini", Price{Input: 0.15, CacheHit: 0.075, Output: 0.60}, 128_000, "https://openai.com/api/pricing/"},
}

// Lookup 按模型 ID 查价。modelID 可带 "provider/" 前缀；大小写不敏感。
func Lookup(modelID string) (Price, bool) {
	e, ok := lookupEntry(modelID)
	return e.Price, ok
}

// ContextWindow 目录登记的上下文窗口；未收录或未登记返回 0。
func ContextWindow(modelID string) int {
	e, _ := lookupEntry(modelID)
	return e.ContextWindow
}

func lookupEntry(modelID string) (Entry, bool) {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	var best Entry
	for _, e := range entries {
		if strings.HasPrefix(id, e.Prefix) && len(e.Prefix) > len(best.Prefix) {
			best = e
		}
	}
	return best, best.Prefix != ""
}

// Entries 返回目录副本，供测试与文档生成。
func Entries() []Entry { return append([]Entry(nil), entries...) }

// CostUSD 按本价目估算费用，口径同 types.CostUSD。
func (p Price) CostUSD(input, cacheHit, output int) float64 {
	return types.CostUSD(p.Input, p.CacheHit, p.Output, input, cacheHit, output)
}

// Resolve 取一次调用适用的单价：先按模型 ID 查目录，查不到回退适配器声明的费率。
func Resolve(modelID string, caps types.ProviderCapabilities) Price {
	if p, ok := Lookup(modelID); ok {
		return p
	}
	return Price{Input: caps.CostPerMTokInput, CacheHit: caps.CostPerMTokCacheHit, Output: caps.CostPerMTokOutput}
}

// CostFor = Resolve(...).CostUSD(...)，供各调用点一行取费。
func CostFor(modelID string, caps types.ProviderCapabilities, input, cacheHit, output int) float64 {
	return Resolve(modelID, caps).CostUSD(input, cacheHit, output)
}

// Cost 按"未命中输入 / 命中输入 / 输出"三段 token 直接计费（调用方已按 Provider 口径拆好 miss）。
func (p Price) Cost(miss, hit, out int) float64 {
	hp := p.CacheHit
	if hp <= 0 {
		hp = p.Input
	}
	return (float64(miss)*p.Input + float64(hit)*hp + float64(out)*p.Output) / 1e6
}
