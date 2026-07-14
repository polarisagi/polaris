package hitl

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// 本文件承载通知分发（ChannelNotifier）与污染事件广播（BroadcastTainted），
// R7 文件行数治理拆分自 gateway.go。

// ChannelNotifier 适配器实现
type ChannelNotifier struct {
	dispatcher protocol.ChannelDispatcher // nil 表示未配置
}

func NewChannelNotifier() *ChannelNotifier {
	return &ChannelNotifier{}
}

// SetDispatcher 注入具体通知 dispatcher（Slack/Email/Desktop）
func (c *ChannelNotifier) SetDispatcher(d protocol.ChannelDispatcher) {
	c.dispatcher = d
}

func (c *ChannelNotifier) Notify(ctx context.Context, msg types.HITLNotification) error {
	slog.Warn("hitl: approval required",
		"checkpoint_id", msg.CheckpointID,
		"task_id", msg.TaskID,
		"description", msg.Description,
		"risk", msg.Risk,
		"review_url", msg.ReviewURL,
		"timeout_ns", msg.Timeout,
	)
	if c.dispatcher == nil {
		// 未配置 dispatcher：通知未送达，返回错误让上层感知（不得静默挂起审批）
		return apperr.New(apperr.CodeInternal,
			"hitl: channel dispatcher not configured; notification logged but not delivered")
	}
	if err := c.dispatcher.Dispatch(ctx, msg); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "hitl: channel dispatcher failed", err)
	}
	return nil
}

// BroadcastTainted 广播污染事件。
func (g *GatewayImpl) BroadcastTainted(ctx context.Context, event string, taintLevel types.TaintLevel) error {
	if taintLevel == types.TaintHigh {
		// High 级别由 HITL 主流程处理，不在此广播
		slog.Warn("hitl: high_taint event suppressed in BroadcastTainted (handled by main HITL flow)",
			"event", event, "taint_level", taintLevel)
		return nil
	}

	slog.Warn("hitl: taint event broadcast",
		"event", event, "taint_level", taintLevel)

	if g.notifier == nil {
		slog.Warn("hitl: notifier not configured, taint broadcast logged only",
			"event", event, "taint_level", taintLevel)
		return nil
	}

	err := g.notifier.Notify(ctx, types.HITLNotification{
		CheckpointID: fmt.Sprintf("taint_%s_%d", event, time.Now().UnixNano()),
		TaskID:       event,
		Description:  fmt.Sprintf("Taint level %d broadcast: %s", int(taintLevel), event),
		Risk:         fmt.Sprintf("taint_level_%d", int(taintLevel)),
		Timeout:      int64(30 * time.Second),
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to broadcast taint via hitl notifier", err)
	}
	return nil
}
