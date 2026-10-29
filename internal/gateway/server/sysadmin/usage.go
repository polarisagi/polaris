package sysadmin

// LLM 用量聚合（ADR-0105 决策八）。
//
//	GET /v1/usage?since=24h&until=&group_by=purpose|model|provider|day
//
// 只读聚合 llm_calls（ADR-0101 决策六的账本），无 LLM 调用。鉴权与其余 /v1 sysadmin 路由同走
// 网关统一中间件，不另设旁路。CLI（polaris usage）与桌面外壳同样只经这条 HTTP 通道（ADR-0096 决策一）。

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// defaultUsageWindow 未给 since 时的默认回看窗口。
const defaultUsageWindow = 24 * time.Hour

// UsageGroupView 是 GET /v1/usage 返回的单个分组（JSON 契约，CLI 复用同一结构）。
type UsageGroupView struct {
	Key               string  `json:"key"`
	Requests          int64   `json:"requests"`
	InputTokens       int64   `json:"input_tokens"`
	CacheHitTokens    int64   `json:"cache_hit_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	ReasoningTokens   int64   `json:"reasoning_tokens"`
	CacheHitRatio     float64 `json:"cache_hit_ratio"`
	CostUSD           float64 `json:"cost_usd"`
	ResponseCacheHits int64   `json:"response_cache_hits"`
}

// UsageReport 是 GET /v1/usage 的响应体。
type UsageReport struct {
	Since   string           `json:"since"` // RFC3339 UTC
	Until   string           `json:"until"`
	GroupBy string           `json:"group_by"`
	Groups  []UsageGroupView `json:"groups"`
	Total   UsageGroupView   `json:"total"`
}

// HandleGetUsage GET /v1/usage
func (h *SysAdminHandler) HandleGetUsage(w http.ResponseWriter, r *http.Request) {
	if h.UsageRepo == nil {
		httputil.RespondError(w, "usage ledger unavailable", nil, http.StatusServiceUnavailable)
		return
	}
	now := time.Now()
	if h.nowFn != nil {
		now = h.nowFn()
	}
	q := r.URL.Query()

	since := now.Add(-defaultUsageWindow)
	if v := q.Get("since"); v != "" {
		t, err := parseUsageTime(v, now)
		if err != nil {
			httputil.RespondError(w, "invalid since (want RFC3339 or a duration like 90m/24h/7d)", err, http.StatusBadRequest)
			return
		}
		since = t
	}
	// until 缺省 = 现在；区间为 [since, until)，缺省时 +1ms 使"此刻刚写入的行"也计入。
	until := now.Add(time.Millisecond)
	if v := q.Get("until"); v != "" {
		t, err := parseUsageTime(v, now)
		if err != nil {
			httputil.RespondError(w, "invalid until (want RFC3339 or a duration like 90m/24h/7d)", err, http.StatusBadRequest)
			return
		}
		until = t
	}
	if !since.Before(until) {
		httputil.RespondError(w, "since must be earlier than until", nil, http.StatusBadRequest)
		return
	}
	groupBy := repo.LLMUsageGroupBy(q.Get("group_by"))
	if groupBy == "" {
		groupBy = repo.LLMUsageByPurpose
	}
	switch groupBy {
	case repo.LLMUsageByPurpose, repo.LLMUsageByModel, repo.LLMUsageByProvider, repo.LLMUsageByDay:
	default:
		httputil.RespondError(w, "invalid group_by (want purpose|model|provider|day)", nil, http.StatusBadRequest)
		return
	}

	groups, err := h.UsageRepo.AggregateLLMUsage(r.Context(), repo.LLMUsageQuery{
		SinceMs: since.UnixMilli(), UntilMs: until.UnixMilli(), GroupBy: groupBy,
	})
	if err != nil {
		httputil.RespondError(w, "usage query failed", err, http.StatusInternalServerError)
		return
	}
	report := UsageReport{
		Since:   since.UTC().Format(time.RFC3339),
		Until:   until.UTC().Format(time.RFC3339),
		GroupBy: string(groupBy),
		Groups:  make([]UsageGroupView, 0, len(groups)),
		Total:   UsageGroupView{Key: "total"},
	}
	var tot repo.LLMUsageGroup
	for _, g := range groups {
		report.Groups = append(report.Groups, toUsageView(g))
		tot.Requests += g.Requests
		tot.ResponseCacheHits += g.ResponseCacheHits
		tot.InputTokens += g.InputTokens
		tot.CacheHitTokens += g.CacheHitTokens
		tot.OutputTokens += g.OutputTokens
		tot.ReasoningTokens += g.ReasoningTokens
		tot.CostUSD += g.CostUSD
	}
	tot.Key = "total"
	report.Total = toUsageView(tot)
	httputil.WriteJSON(w, report)
}

// toUsageView 由求和量派生缓存命中率。
//
// 口径：cache_hit_ratio = cache_hit_tokens / input_tokens。input_tokens 已由 usage_recorder.fillUsage
// 归一为"全部输入，含缓存命中"（OpenAI/DeepSeek/Google 的 prompt_tokens 本就含命中；Anthropic 的
// input_tokens 不含命中，记账时已加回 cache_read + cache_creation），因此分母是全部输入、比率天然 ≤1。
// 归一落地前写入的历史 Anthropic 行仍是旧口径，可能使比率 >1，故夹到 1，避免图表出现 >100%。
// 精确响应缓存命中的行（status=cache_hit）token 为 0，不进分母，只体现在 response_cache_hits。
func toUsageView(g repo.LLMUsageGroup) UsageGroupView {
	ratio := 0.0
	if g.InputTokens > 0 {
		ratio = min(float64(g.CacheHitTokens)/float64(g.InputTokens), 1)
	}
	return UsageGroupView{
		Key: g.Key, Requests: g.Requests, InputTokens: g.InputTokens, CacheHitTokens: g.CacheHitTokens,
		OutputTokens: g.OutputTokens, ReasoningTokens: g.ReasoningTokens, CacheHitRatio: ratio,
		CostUSD: g.CostUSD, ResponseCacheHits: g.ResponseCacheHits,
	}
}

// parseUsageTime 解析 since/until：RFC3339 绝对时刻，或相对"多久之前"的时长（"90m"、"24h"、"7d"）。
func parseUsageTime(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return time.Time{}, apperr.New(apperr.CodeInvalidInput, "want RFC3339, or a duration like 90m/24h/7d")
		}
		return now.Add(-time.Duration(n) * 24 * time.Hour), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return time.Time{}, apperr.New(apperr.CodeInvalidInput, "want RFC3339, or a duration like 90m/24h/7d")
	}
	return now.Add(-d), nil
}
