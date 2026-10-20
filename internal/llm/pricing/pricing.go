// Package pricing 是模型单价目录（USD / 百万 token），llm_calls 记账与历史迁移的单一事实源。
//
// 为什么独立成表：用户常把 DeepSeek 等模型接在通用 openai_compat 适配器下，适配器自身只知道
// OpenAI 的价格，按适配器类型取价会把 DeepSeek 流量按 gpt-4o-mini 计费。费用必须按"实际
// 调用的模型 ID"取价，所以记账时先查本表，查不到才回退适配器声明的 ProviderCapabilities。
package pricing

import (
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// Price 单位：USD / 百万 token。CacheHit<=0 表示无单独命中价（按 Input 计）。
type Price struct {
	Input    float64
	CacheHit float64
	Output   float64
}

// scaled 返回三档单价均乘以 f 的新价（非高峰折扣用）。
func (p Price) scaled(f float64) Price {
	return Price{Input: p.Input * f, CacheHit: p.CacheHit * f, Output: p.Output * f}
}

// Entry 一条目录项。Prefix 按小写前缀匹配（取最长者），Source 为价格出处。
type Entry struct {
	Prefix string
	Price  Price
	// ContextWindow 上下文窗口（token），0 = 未知。状态栏的"上下文占用"以它为分母；
	// 优先用探测到的目录（provider_discovered_models），这里是目录缺失时的回退。
	ContextWindow int
	Source        string
	// OffPeakRatio 非高峰时段相对 Price（高峰价）的比例；0 = 该模型不分时段（全天按 Price 计）。
	OffPeakRatio float64
}

// deepseekSource 官方定价页。页面注明高峰/非高峰两档：非高峰价为高峰价的一半；
// 高峰 = UTC 周一至周五 01:00–04:00 与 06:00–10:00（中国法定节假日除外），其余时间含周末为非高峰。
// Entry.Price 存高峰价（作 Lookup 的"标价"与路由成本评分的上界），按调用时刻折算见 LookupAt。
// 页面只写 "$" 符号、未点明币种，按 USD 存储。核实日期 2026-10-20。
const deepseekSource = "https://api-docs.deepseek.com/quick_start/pricing"

// deepseekOffPeakRatio 非高峰价 / 高峰价。
const deepseekOffPeakRatio = 0.5

// entries 顺序无关，Lookup 取最长前缀。
func entries() []Entry {
	return []Entry{
		{"deepseek-flash", Price{Input: 0.30, CacheHit: 0.006, Output: 1.20}, 1_000_000, deepseekSource, deepseekOffPeakRatio},
		// 官方页说明 deepseek-v4-flash 仍可用且按 Flash 计价。
		{"deepseek-v4-flash", Price{Input: 0.30, CacheHit: 0.006, Output: 1.20}, 1_000_000, deepseekSource, deepseekOffPeakRatio},
		{"deepseek-v4-pro", Price{Input: 1.32, CacheHit: 0.044, Output: 3.96}, 1_000_000, deepseekSource, deepseekOffPeakRatio},
		{"gpt-4o-mini", Price{Input: 0.15, CacheHit: 0.075, Output: 0.60}, 128_000, "https://openai.com/api/pricing/", 0},
	}
}

// IsPeak 判断 at 是否落在 DeepSeek 高峰时段：UTC 周一至周五 [01:00,04:00) 与 [06:00,10:00)。
// 以 UTC 判定，与 at 的时区无关。
//
// 已知偏差：官方规定"中国法定节假日除外"（节假日全天非高峰），这里不建模节假日历，
// 节假日的工作日会被判为高峰——费用高估而不低估，预算护栏更安全。
func IsPeak(at time.Time) bool {
	u := at.UTC()
	switch u.Weekday() {
	case time.Saturday, time.Sunday:
		return false
	}
	h := u.Hour()
	return (h >= 1 && h < 4) || (h >= 6 && h < 10)
}

// Lookup 按模型 ID 查"标价"（高峰价）。modelID 可带 "provider/" 前缀；大小写不敏感。
// 记账、重算须用 LookupAt（按调用时刻分时段）；Lookup 只用于不关心时刻的上界场景（路由成本评分）。
func Lookup(modelID string) (Price, bool) {
	e, ok := lookupEntry(modelID)
	return e.Price, ok
}

// LookupAt 按模型 ID 与调用开始时刻 at 查价：分时段计价的模型在非高峰时段返回高峰价 × OffPeakRatio。
func LookupAt(modelID string, at time.Time) (Price, bool) {
	e, ok := lookupEntry(modelID)
	if !ok {
		return e.Price, false
	}
	if e.OffPeakRatio > 0 && !IsPeak(at) {
		return e.Price.scaled(e.OffPeakRatio), true
	}
	return e.Price, true
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
	for _, e := range entries() {
		if strings.HasPrefix(id, e.Prefix) && len(e.Prefix) > len(best.Prefix) {
			best = e
		}
	}
	return best, best.Prefix != ""
}

// Entries 返回目录副本，供测试与文档生成。
func Entries() []Entry { return entries() }

// CostUSD 按本价目估算费用，口径同 types.CostUSD。
func (p Price) CostUSD(input, cacheHit, output int) float64 {
	return types.CostUSD(p.Input, p.CacheHit, p.Output, input, cacheHit, output)
}

// Resolve 取一次调用适用的单价：先按模型 ID 与调用开始时刻 at 查目录，查不到回退适配器声明的费率。
func Resolve(modelID string, caps types.ProviderCapabilities, at time.Time) Price {
	if p, ok := LookupAt(modelID, at); ok {
		return p
	}
	return Price{Input: caps.CostPerMTokInput, CacheHit: caps.CostPerMTokCacheHit, Output: caps.CostPerMTokOutput}
}

// CostFor = Resolve(...).CostUSD(...)，供各调用点一行取费。at 为调用开始时刻。
func CostFor(modelID string, caps types.ProviderCapabilities, at time.Time, input, cacheHit, output int) float64 {
	return Resolve(modelID, caps, at).CostUSD(input, cacheHit, output)
}

// Cost 按"未命中输入 / 命中输入 / 输出"三段 token 直接计费（调用方已按 Provider 口径拆好 miss）。
func (p Price) Cost(miss, hit, out int) float64 {
	hp := p.CacheHit
	if hp <= 0 {
		hp = p.Input
	}
	return (float64(miss)*p.Input + float64(hit)*hp + float64(out)*p.Output) / 1e6
}
