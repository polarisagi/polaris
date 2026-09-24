package agent

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestAbortTurn_EmitsErrorAndTaskDone 复现 2026-09-25 实测：Dispatch 错误使 Run()
// 直接返回，订阅方收不到原因也等不到 task_done，客户端挂到超时；FSM 停在非终态，
// Pool 下一轮会把输入投给已无 Run() 的旧实例。
func TestAbortTurn_EmitsErrorAndTaskDone(t *testing.T) {
	a := NewAgentWithDefaults("test-abort")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sub := a.SubscribeStream(ctx)

	a.abortTurn(ctx, apperr.New(apperr.CodeInternal, "no transition from 2 with trigger 10"))

	var gotErr, gotDone bool
	for len(sub) > 0 {
		ev := <-sub
		switch {
		case ev.Type == types.AgentStreamEventError:
			gotErr = true
		case ev.Type == types.AgentStreamEventStatus && ev.Content == "task_done":
			gotDone = true
		}
	}
	if !gotErr || !gotDone {
		t.Fatalf("回合中止必须同时发出错误与 task_done：err=%v done=%v", gotErr, gotDone)
	}
	if a.sm.Current() != types.AgentStateFailed {
		t.Fatalf("中止后必须处于 S_FAILED（Pool 据此换新实例），实际 %v", a.sm.Current())
	}
}
