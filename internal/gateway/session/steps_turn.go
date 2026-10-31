package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type recorderCtxKey struct{}

func withRecorder(ctx context.Context, r *stepRecorder) context.Context {
	return context.WithValue(ctx, recorderCtxKey{}, r)
}

func recorderFrom(ctx context.Context) *stepRecorder {
	r, _ := ctx.Value(recorderCtxKey{}).(*stepRecorder)
	return r
}

// turnIDSetter 内核控制器的可选能力：接收网关侧的回合 ID，使 llm_calls.turn_id 与步骤、消息同值。
type turnIDSetter interface{ SetTurnID(id string) }

// beginTurnSteps 为本回合建立步骤记录器并把回合 ID 交给内核。回合 ID 优先取 ctx（TurnHub 注入），
// 缺省（单测、非 TurnHub 入口）才自己生成。返回的 ctx 携带记录器；finish 须被调用（defer 即可，幂等）。
func (o *orchestrator) beginTurnSteps(ctx context.Context, sink Sink, sessionID string, agentCtrl protocol.AgentController) (context.Context, string, *stepRecorder) {
	turnID := protocol.TurnIDFrom(ctx)
	if turnID == "" {
		turnID = newTurnID()
	}
	if s, ok := agentCtrl.(turnIDSetter); ok {
		s.SetTurnID(turnID)
	}
	rec := newStepRecorder(ctx, sink, o.turns, sessionID, turnID)
	return withRecorder(ctx, rec), turnID, rec
}

// usagePollInterval/usagePollMax：llm_calls 由异步写协程落库，回合刚结束时最后一次调用的记账行可能还在队列里。
// 轮询到调用数稳定（连续两次相同）或超时为止；超时只会让摘要少算最后一条，不影响回合。
const (
	usagePollInterval = 50 * time.Millisecond
	usagePollMax      = 600 * time.Millisecond
)

func (o *orchestrator) awaitTurnUsage(ctx context.Context, turnID string) TurnUsageSummary {
	if o.turns == nil {
		return TurnUsageSummary{}
	}
	deadline := time.Now().Add(usagePollMax)
	var prev TurnUsageSummary
	for i := 0; ; i++ {
		cur, err := o.turns.TurnUsage(ctx, turnID)
		if err != nil {
			slog.Warn("session: turn usage query failed", "turn", turnID, "error_meta", safeErrorSummary(err))
			return cur
		}
		if i > 0 && cur.Calls == prev.Calls {
			return cur
		}
		prev = cur
		if time.Now().After(deadline) {
			return cur
		}
		select {
		case <-time.After(usagePollInterval):
		case <-ctx.Done():
			return cur
		}
	}
}

// finishTurnMeta 回合收尾：收口步骤、汇总用量、把 turn_id 与用量摘要写到 assistant 消息上，
// 返回 complete 事件要附带的字段。messageID<=0（消息未落库）时只返回事件字段，不写库。
func (o *orchestrator) finishTurnMeta(ctx context.Context, rec *stepRecorder, turnID string, messageID, durationMs int64, aborted bool) map[string]any {
	extra := map[string]any{"turn_id": turnID}
	if rec == nil {
		return extra
	}
	summary := rec.finalize(aborted)
	// finalize 已把完整诊断写入私有步骤存储；complete 事件只发安全投影。
	extra["steps"] = publicStepSummary(summary)
	u := o.awaitTurnUsage(ctx, turnID)
	usage := map[string]any{
		"input": u.Input, "cache_hit": u.CacheHit, "output": u.Output,
		"cost_usd": u.CostUSD, "llm_calls": u.Calls, "duration_ms": durationMs,
	}
	extra["usage"] = usage
	if o.turns != nil && messageID > 0 {
		raw, err := json.Marshal(usage)
		if err == nil {
			if err := o.turns.AttachTurnMeta(ctx, messageID, turnID, string(raw)); err != nil {
				slog.Warn("session: attach turn meta failed", "message_id", messageID, "turn", turnID, "error_meta", safeErrorSummary(err))
			}
		}
	}
	return extra
}

// friendlyReasons 失败原因归类表：按顺序匹配，第一个命中关键词（小写子串）的条目胜出。
func friendlyReasons() []struct {
	reason   string
	keywords []string
} {
	return []struct {
		reason   string
		keywords []string
	}{
		{"模型响应超时", []string{"deadline", "timeout", "超时"}},
		{"模型凭据无效或无权限", []string{"401", "403", "unauthorized", "api key", "api_key", "invalid_api"}},
		{"模型触发限流", []string{"429", "rate limit", "rate_limit", "限流"}},
		{"模型额度不足", []string{"quota", "insufficient", "余额", "额度"}},
		{"模型没有返回内容", []string{"空内容", "empty"}},
	}
}

// friendlyTurnError 把回合失败的原始错误归类成给人看的一句话原因。原始错误另存 error_detail。
// 只做关键词归类，不解析 Provider 私有错误格式；归不了类就给通用说法。code 暂不参与归类，
// 留作与其他失败来源（no_provider 等）扩展时区分。
func friendlyTurnError(_, detail string) string {
	d := strings.ToLower(detail)
	for _, r := range friendlyReasons() {
		for _, k := range r.keywords {
			if strings.Contains(d, k) {
				return r.reason
			}
		}
	}
	return "模型调用失败"
}

// failTurn 回合以失败收场：在步骤时间线里记下错误、把回合 ID 挂到触发它的用户消息上，
// 使会话历史能在该用户消息之后显示"本轮失败"占位（否则失败回合只剩一条孤零零的用户消息，
// 重发后看起来像同一问题被提交了两次）。userMsgID<=0（用户消息未落库）时只记步骤。
// 这里写的全是展示数据：不触碰 chat_messages 的内容列，不进入模型历史。
func (o *orchestrator) failTurn(ctx context.Context, rec *stepRecorder, turnID string, userMsgID int64, kind string, errorClass apperr.Code, detail string) {
	if rec == nil {
		return
	}
	rec.fail(kind, errorClass, friendlyTurnError(kind, detail), detail)
	if o.turns != nil && userMsgID > 0 {
		if err := o.turns.AttachTurnMeta(context.WithoutCancel(ctx), userMsgID, turnID, ""); err != nil {
			slog.Warn("session: attach failed turn to user message", "message_id", userMsgID, "turn", turnID, "error_meta", safeErrorSummary(err))
		}
	}
}
