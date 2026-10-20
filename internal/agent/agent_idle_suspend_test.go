package agent

import (
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// E1 回归：空闲计时器只在 S_IDLE 才能挂起。回合内长耗时工具/等待审批超过 IdleTimeoutSec 时，
// 计时器触发不得把 FSM 强行置为 S_SUSPENDED，否则工具返回后的 TriggerLoopExecDone
// 会报 "no transition from 11 with trigger 24"（11=S_SUSPENDED，24=TriggerLoopExecDone）。
func TestIdleTimer_DoesNotSuspendMidTurn(t *testing.T) {
	slowApprove := func(types.HITLPrompt) types.HITLResponse {
		time.Sleep(2500 * time.Millisecond) // 超过下面 1s 的空闲超时
		return types.HITLResponse{Approved: true, OptionKey: "approve"}
	}
	f := newPermFixture(t, types.ModeAutoReview, "p1", &memRules{}, []*types.ProviderResponse{
		{ToolCalls: []types.InferToolCall{call("c1", "bash", `{"command":"frobnicate --alpha"}`)}},
		{Content: "done"},
	}, slowApprove)
	f.ag.Config.IdleTimeoutSec = 1

	runAgenticTurnWait(t, f.ag)

	if got := f.ag.sm.Current(); got != types.AgentStateComplete {
		t.Fatalf("turn must complete after slow approval, final state = %v", got)
	}
	if f.exec.ranCount("bash") != 1 {
		t.Fatal("approved tool should have executed")
	}
}
