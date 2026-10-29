package cronadmin

import (
	"context"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// ApprovalPollInterval 无人值守运行轮询“是否在等审批”的间隔。
const ApprovalPollInterval = 2 * time.Second

// pendingLister 是 HITL 网关的可选能力：列出待审批项（hitl.GatewayImpl 实现）。
type pendingLister interface {
	Pending(ctx context.Context) ([]types.HITLPrompt, error)
}

// sessionWaitingForApproval 返回“本运行的会话是否有无人值守审批在等用户”。
// 网关不支持 Pending 或查询失败时按“没在等”处理：退化为原先的固定超时，不会让运行永远不超时。
func sessionWaitingForApproval(gw HITLGateway, sessionID string) func(context.Context) bool {
	return PendingMatcher(gw, func(p types.HITLPrompt) bool { return p.Unattended && p.SessionID == sessionID })
}

// PendingMatcher 返回“待审批队列里是否存在满足 match 的项”的判定函数，供各类后台运行（cron/webhook 自动化、
// 工作流）共用同一套“审批等待期间暂停超时预算”机制。gw 为 nil、不支持 Pending 或查询失败一律按“没在等”处理——
// 退化为固定超时，宁可超时也不让运行永远不超时。
func PendingMatcher(gw any, match func(types.HITLPrompt) bool) func(context.Context) bool {
	pl, ok := gw.(pendingLister)
	if !ok || gw == nil {
		return func(context.Context) bool { return false }
	}
	return func(ctx context.Context) bool {
		pend, err := pl.Pending(ctx)
		if err != nil {
			return false
		}
		for _, p := range pend {
			if match(p) {
				return true
			}
		}
		return false
	}
}

// SuperviseBudget 用“可暂停的预算”替代固定 context.WithTimeout：
// 运行在等待用户审批（可长达 24 小时）时不消耗预算，否则 5-60 分钟的运行超时会在批准之前先把运行杀掉，
// 审批形同虚设。预算耗尽调用 cancel；ctx 结束时返回。
// onChange 在“等待审批”状态翻转时回调（用于把运行状态切到 suspended / running）。
func SuperviseBudget(
	ctx context.Context, cancel context.CancelFunc, budget, poll time.Duration,
	waiting func(context.Context) bool, onChange func(waiting bool),
) {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	var used time.Duration
	last := time.Now()
	wasWaiting := false
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			elapsed := now.Sub(last)
			last = now
			w := waiting(ctx)
			if w != wasWaiting {
				wasWaiting = w
				if onChange != nil {
					onChange(w)
				}
			}
			if w {
				continue
			}
			used += elapsed
			if used >= budget {
				slog.Warn("automation: run budget exhausted", "budget", budget)
				cancel()
				return
			}
		}
	}
}
