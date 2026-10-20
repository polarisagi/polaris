package workflowadmin

import (
	"context"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/server/sysadmin/cronadmin"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// 工作流的整体 / 单步超时同 cron 自动化一样要“可暂停”：运行在等待人工审批（可长达 24 小时）时不消耗预算，
// 否则 30 分钟的步骤超时、120 分钟的整体超时会在用户批准之前先把运行杀掉，审批形同虚设。
// 判定“在等审批”的两类来源：
//   - 步骤内 Agent 回合的无人值守工具审批：Unattended 且 SessionID 是本运行某步骤的会话；
//   - 写入类管线工具步骤的审批：ID 形如 pipeline_tool_step:<runID>:<stepKey>（见 hitlApprover）。

const pipelineApprovalPrefix = "pipeline_tool_step:"

// trackSession 登记某步骤会话属于哪个运行（供整体超时判定“本运行是否有步骤在等审批”），返回注销函数。
func (h *WorkflowAdmin) trackSession(sessionID, runID string) (untrack func()) {
	if sessionID == "" || runID == "" {
		return func() {}
	}
	h.stepSessions.Store(sessionID, runID)
	return func() { h.stepSessions.Delete(sessionID) }
}

// runWaiting 本运行是否有任一步骤在等审批（用于整体超时）。
func (h *WorkflowAdmin) runWaiting(runID string) func(context.Context) bool {
	return cronadmin.PendingMatcher(h.HITL, func(p types.HITLPrompt) bool {
		if strings.HasPrefix(p.ID, pipelineApprovalPrefix+runID+":") {
			return true
		}
		if !p.Unattended {
			return false
		}
		owner, ok := h.stepSessions.Load(p.SessionID)
		return ok && owner == runID
	})
}

// stepWaiting 单个步骤是否在等审批（用于单步超时）。sessionID 为空表示管线步骤（无 Agent 会话）。
func (h *WorkflowAdmin) stepWaiting(runID, stepKey, sessionID string) func(context.Context) bool {
	return cronadmin.PendingMatcher(h.HITL, func(p types.HITLPrompt) bool {
		if p.ID == pipelineApprovalPrefix+runID+":"+stepKey {
			return true
		}
		return sessionID != "" && p.Unattended && p.SessionID == sessionID
	})
}

// pausableTimeout 返回带“可暂停预算”的 ctx：budget 只在 waiting 为 false 时累计，耗尽则取消 ctx。
// 替代 context.WithTimeout；调用方 defer 返回的 cancel（它同时停止监督协程）。
func pausableTimeout(budget time.Duration, waiting func(context.Context) bool) (context.Context, context.CancelFunc) {
	return pausableTimeoutPoll(budget, cronadmin.ApprovalPollInterval, waiting)
}

func pausableTimeoutPoll(budget, poll time.Duration, waiting func(context.Context) bool) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	concurrent.SafeGo(context.Background(), "workflowadmin.pausable_budget", func(context.Context) {
		cronadmin.SuperviseBudget(ctx, cancel, budget, poll, waiting, nil)
	})
	return ctx, cancel
}
