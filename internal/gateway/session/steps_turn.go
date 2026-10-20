package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
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
			slog.Warn("session: turn usage query failed", "turn", turnID, "err", err)
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
	extra["steps"] = summary
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
				slog.Warn("session: attach turn meta failed", "message_id", messageID, "turn", turnID, "err", err)
			}
		}
	}
	return extra
}
