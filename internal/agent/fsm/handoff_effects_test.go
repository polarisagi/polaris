package fsm

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestHandoffTransitions_EmitEffects 委派的挂起与恢复都靠 executeDeterministicEffect 按当前状态拦截
// （启动 watcher / 重新进入 runExecuteDAG）。转移若不产出效应，拦截点根本不会被调用：
// transfer_to_agent 之后父任务永远停在 S_AWAIT_AGENT。
func TestHandoffTransitions_EmitEffects(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	for _, tc := range []struct {
		from    types.AgentState
		trigger types.AgentTrigger
	}{
		{types.AgentStateExecute, types.TriggerAwaitAgent},
		{types.AgentStateAwaitAgent, types.TriggerAgentHandoffDone},
	} {
		tr, ok := sm.transitions[tc.from][tc.trigger]
		if !ok {
			t.Fatalf("missing transition %v --%v-->", tc.from, tc.trigger)
		}
		effs, err := tr.Effects(context.Background(), &StateContext{})
		if err != nil || len(effs) != 1 {
			t.Fatalf("%v --%v--> must emit exactly one effect, got %d (%v)", tc.from, tc.trigger, len(effs), err)
		}
		if _, ok := effs[0].(protocol.DeterministicEffect); !ok {
			t.Fatalf("%v --%v--> must emit a DeterministicEffect, got %T", tc.from, tc.trigger, effs[0])
		}
	}
}
