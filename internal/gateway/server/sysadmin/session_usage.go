package sysadmin

// 会话级用量（A3/A4）：
//
//	GET /v1/sessions/{sessionID}/usage → 本会话全部用途（kernel、persona_refine、压缩……）的累计与按回合明细
//	GET /v1/status?session_id=…        → 同一份数据压成状态栏所需的少量字段（server 包调用 BuildSessionUsage）
//
// 币种一律 USD（单价 USD/百万 token，见 internal/llm/pricing）；折算成 ¥/$ 是客户端展示层的事。

import (
	"context"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/gateway/session"
	"github.com/polarisagi/polaris/internal/llm/pricing"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// UsageTotalsView 一组调用的累计量。
type UsageTotalsView struct {
	Input         int64   `json:"input"`
	CacheHit      int64   `json:"cache_hit"`
	Output        int64   `json:"output"`
	Reasoning     int64   `json:"reasoning"`
	CostUSD       float64 `json:"cost_usd"`
	Calls         int64   `json:"calls"`
	CacheHitRatio float64 `json:"cache_hit_ratio"`
}

// TurnUsageView 一个回合的用量。
type TurnUsageView struct {
	TurnID     string `json:"turn_id"`
	DurationMs int64  `json:"duration_ms"` // 回合内首次到最后一次 LLM 调用之间的跨度，仅作参考
	UsageTotalsView
}

// SessionUsageView 是 GET /v1/sessions/{id}/usage 的响应体。
type SessionUsageView struct {
	SessionID string `json:"session_id"`
	UsageTotalsView
	// 上下文占用：最近一次内核请求的输入 token / 模型上下文窗口。窗口未知时 ContextWindow=0、ContextPct=0。
	LastInput     int64           `json:"last_input"`
	ContextWindow int             `json:"context_window"`
	ContextPct    float64         `json:"context_pct"`
	ModelID       string          `json:"model_id"`
	ByTurn        []TurnUsageView `json:"by_turn"`
}

func totalsView(t repo.LLMUsageTotals) UsageTotalsView {
	v := UsageTotalsView{
		Input: t.InputTokens, CacheHit: t.CacheHitTokens, Output: t.OutputTokens, Reasoning: t.ReasoningTokens,
		CostUSD: t.CostUSD, Calls: t.Calls,
	}
	if t.InputTokens > 0 {
		v.CacheHitRatio = min(float64(t.CacheHitTokens)/float64(t.InputTokens), 1)
	}
	return v
}

// BuildSessionUsage 查询并组装一个会话的用量视图。sessionID 为空时返回自 todayStart 起的全局累计
// （状态栏在没有当前会话时显示"今日"）。q 为 nil 返回零值视图。
func BuildSessionUsage(ctx context.Context, q repo.LLMUsageQueryRepository, sessionID string, todayStart time.Time) (SessionUsageView, error) {
	view := SessionUsageView{SessionID: sessionID, ByTurn: []TurnUsageView{}}
	sq, ok := q.(repo.LLMSessionUsageRepository)
	if !ok {
		return view, nil
	}
	if sessionID == "" {
		t, err := sq.UsageSince(ctx, todayStart.UnixMilli())
		if err != nil {
			return view, apperr.Wrap(apperr.CodeInternal, "BuildSessionUsage: today", err)
		}
		view.UsageTotalsView = totalsView(t)
		return view, nil
	}
	su, err := sq.SessionUsage(ctx, sessionID)
	if err != nil {
		return view, apperr.Wrap(apperr.CodeInternal, "BuildSessionUsage: session", err)
	}
	view.UsageTotalsView = totalsView(su.LLMUsageTotals)
	view.LastInput, view.ModelID = su.LastInputTokens, su.LastModelID
	if view.ModelID != "" {
		cw, cwErr := sq.ContextWindowFor(ctx, view.ModelID)
		if cwErr != nil || cw <= 0 {
			cw = pricing.ContextWindow(view.ModelID)
		}
		view.ContextWindow = cw
		if cw > 0 {
			view.ContextPct = float64(view.LastInput) / float64(cw)
		}
	}
	for _, tu := range su.ByTurn {
		view.ByTurn = append(view.ByTurn, TurnUsageView{
			TurnID: tu.TurnID, DurationMs: tu.LastAtMs - tu.FirstAtMs, UsageTotalsView: totalsView(tu.LLMUsageTotals),
		})
	}
	return view, nil
}

// HandleGetSessionUsage GET /v1/sessions/{sessionID}/usage
func (h *SysAdminHandler) HandleGetSessionUsage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("sessionID")
	if !session.SessionIDPattern.MatchString(sessionID) {
		httputil.RespondError(w, "invalid session_id", apperr.New(apperr.CodeInvalidInput, "invalid session_id"), http.StatusBadRequest)
		return
	}
	if h.UsageRepo == nil {
		httputil.RespondError(w, "usage ledger unavailable", nil, http.StatusServiceUnavailable)
		return
	}
	view, err := BuildSessionUsage(r.Context(), h.UsageRepo, sessionID, time.Time{})
	if err != nil {
		httputil.RespondError(w, "usage query failed", err, http.StatusInternalServerError)
		return
	}
	httputil.WriteJSON(w, view)
}
