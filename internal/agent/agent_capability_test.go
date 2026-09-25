package agent

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/token"
)

// TestWithJITCapability_OneShot M07 §6：通过闸门后 JIT 签发、只对本次调用有效。
// 此前通用工具路径从不签发，写类工具与 trust<3 工具在执行闸门必被拒绝。
func TestWithJITCapability_OneShot(t *testing.T) {
	a := &Agent{ID: "agent-x"}
	args := []byte(`{"path":"a.txt"}`)
	a.recordValidatedPlan(&protocol.DAGPlan{Nodes: []protocol.ExecNode{{ID: "n1", ToolName: "write_file", Args: args}}})
	ctx, revoke, err := a.withJITCapability(context.Background(), "write_file", args)
	if err != nil {
		t.Fatalf("mint failed: %v", err)
	}
	tok, ok := ctx.Value(protocol.CtxCapabilityTokenKey{}).(*token.Token)
	if !ok || tok == nil {
		t.Fatal("令牌必须经 CtxCapabilityTokenKey 注入本次调用的 ctx")
	}
	if err := action.GetTokenManager().Verify(tok); err != nil {
		t.Fatalf("调用期间令牌应可被执行闸门验签通过: %v", err)
	}
	revoke()
	if err := action.GetTokenManager().Verify(tok); err == nil {
		t.Fatal("调用结束撤销后令牌不得再被复用")
	}
}

// TestWithJITCapability_OnlyForValidatedCalls 令牌只为已通过 S_VALIDATE 的计划节点签发：
// 无计划、工具名不在计划中、参数与校验时不一致，一律拒签（此前对任意工具名无条件签发，
// 执行闸门的令牌要求对 trust<3 工具恒真）。
func TestWithJITCapability_OnlyForValidatedCalls(t *testing.T) {
	args := []byte(`{"path":"a.txt"}`)
	cases := []struct {
		name     string
		plan     *protocol.DAGPlan
		tool     string
		callArgs []byte
	}{
		{"无已校验计划", nil, "write_file", args},
		{"工具不在计划中", &protocol.DAGPlan{Nodes: []protocol.ExecNode{{ID: "n1", ToolName: "read_file", Args: args}}}, "write_file", args},
		{"参数与校验时不一致", &protocol.DAGPlan{Nodes: []protocol.ExecNode{{ID: "n1", ToolName: "write_file", Args: args}}}, "write_file", []byte(`{"path":"b.txt"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{ID: "agent-x"}
			if tc.plan != nil {
				a.recordValidatedPlan(tc.plan)
			}
			ctx, _, err := a.withJITCapability(context.Background(), tc.tool, tc.callArgs)
			if err == nil {
				t.Fatal("未经校验的调用不得签发令牌")
			}
			if ctx.Value(protocol.CtxCapabilityTokenKey{}) != nil {
				t.Fatal("拒签时 ctx 不得携带令牌")
			}
		})
	}
}

// TestWithTaskScopeCtx_BackgroundMarker 经 AcquireHeadless 获取的 Agent，其 effect ctx
// 须标记为后台工作，LLM 额度才会按可降级优先级申请；交互获取须清除标记。
func TestWithTaskScopeCtx_BackgroundMarker(t *testing.T) {
	a := &Agent{ID: "agent-x"}
	a.background.Store(true)
	if !protocol.IsBackgroundWork(a.withTaskScopeCtx(context.Background())) {
		t.Fatal("headless 获取的 Agent effect ctx 应带后台标记")
	}
	a.background.Store(false)
	if protocol.IsBackgroundWork(a.withTaskScopeCtx(context.Background())) {
		t.Fatal("交互获取的 Agent effect ctx 不得带后台标记")
	}
}
