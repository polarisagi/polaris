package workflowadmin

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

type pendingGW struct{ pending atomic.Value } // []types.HITLPrompt

func (g *pendingGW) Prompt(context.Context, types.HITLPrompt) (*types.HITLResponse, error) {
	return nil, nil
}
func (g *pendingGW) Pending(context.Context) ([]types.HITLPrompt, error) {
	v, _ := g.pending.Load().([]types.HITLPrompt)
	return v, nil
}

// 审批等待期间预算不消耗；批准（队列清空）后继续计时直至耗尽。
func TestPausableTimeout_PausesWhileWaiting(t *testing.T) {
	var waiting atomic.Bool
	waiting.Store(true)
	ctx, cancel := pausableTimeoutPoll(80*time.Millisecond, 5*time.Millisecond, func(context.Context) bool { return waiting.Load() })
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatal("等待审批期间不应超时")
	case <-time.After(250 * time.Millisecond):
	}
	waiting.Store(false)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("审批结束后预算耗尽应取消 ctx")
	}
}

func TestWorkflowWaitingPredicates(t *testing.T) {
	gw := &pendingGW{}
	h := &WorkflowAdmin{HITL: gw}
	defer h.trackSession("sess-a", "run-1")()

	gw.pending.Store([]types.HITLPrompt{{ID: "x1", Unattended: true, SessionID: "sess-other"}})
	if h.runWaiting("run-1")(context.Background()) || h.stepWaiting("run-1", "s1", "sess-a")(context.Background()) {
		t.Fatal("他人的审批不应让本运行/步骤暂停")
	}

	gw.pending.Store([]types.HITLPrompt{{ID: "x2", Unattended: true, SessionID: "sess-a"}})
	if !h.runWaiting("run-1")(context.Background()) {
		t.Error("本运行某步骤会话在等审批，整体预算应暂停")
	}
	if h.runWaiting("run-2")(context.Background()) {
		t.Error("其他运行不应受影响")
	}
	if !h.stepWaiting("run-1", "s1", "sess-a")(context.Background()) {
		t.Error("步骤自己的会话在等审批，单步预算应暂停")
	}

	// 写入类管线工具步骤的审批按 ID 约定匹配。
	gw.pending.Store([]types.HITLPrompt{{ID: "pipeline_tool_step:run-1:s9"}})
	if !h.runWaiting("run-1")(context.Background()) || !h.stepWaiting("run-1", "s9", "")(context.Background()) {
		t.Error("管线步骤审批应使整体与对应单步预算暂停")
	}
	if h.stepWaiting("run-1", "s2", "")(context.Background()) {
		t.Error("别的步骤不应暂停")
	}
}
