package agent

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/pkg/types"
)

// reviewGateValidator 模拟 L1_taint：节点参数未经人工复核即拦截（与 execute/dag
// validateNodeTaint 的 SanitizeByUserReview 判据同口径）。
type reviewGateValidator struct{ calls int }

func (v *reviewGateValidator) Validate(_ context.Context, vCtx *protocol.DAGValidationContext) error {
	v.calls++
	for _, n := range vCtx.Plan.Nodes {
		if !vCtx.ReviewChecker.IsReviewed(vCtx.AgentID, n.Args) {
			return &protocol.DAGValidationError{Layer: "L1_taint", NodeID: n.ID, Reason: "TaintHigh args blocked"}
		}
	}
	return nil
}

// mintingHITL 模拟 hitl.GatewayImpl：批准时按 ExemptionFieldContent 铸造豁免令牌。
type mintingHITL struct {
	vault   *token.ExemptionVault
	approve bool
	prompts []types.HITLPrompt
}

func (h *mintingHITL) Prompt(_ context.Context, p types.HITLPrompt) (*types.HITLResponse, error) {
	h.prompts = append(h.prompts, p)
	if h.approve {
		h.vault.Store(p.AgentID, token.NewTaintExemptionToken(p.ExemptionFieldContent, time.Minute, "user"))
	}
	return &types.HITLResponse{Approved: h.approve}, nil
}
func (h *mintingHITL) Respond(context.Context, string, types.HITLResponse) error { return nil }
func (h *mintingHITL) Pending(context.Context) ([]types.HITLPrompt, error)       { return nil, nil }

func newTaintReviewFixture(t *testing.T, approve bool) (*Agent, *mintingHITL, *reviewGateValidator, *protocol.DAGValidationContext) {
	t.Helper()
	a := NewAgent("a1", nil, nil)
	a.sCtx.AgentID = "a1"
	vault := token.NewExemptionVault()
	h := &mintingHITL{vault: vault, approve: approve}
	v := &reviewGateValidator{}
	a.hitl = h
	a.dagValidator = v
	vCtx := &protocol.DAGValidationContext{
		AgentID:          "a1",
		ActiveTaintLevel: types.TaintHigh,
		ReviewChecker:    vault,
		Plan: &protocol.DAGPlan{Nodes: []protocol.ExecNode{
			{ID: "n1", ToolName: "write_file", Args: []byte(`{"path":"a.txt"}`)},
			{ID: "n2", ToolName: "write_file", Args: []byte(`{"path":"b.txt"}`)},
		}},
	}
	return a, h, v, vCtx
}

// 批准后重新校验放行；多节点各送审一次，后一次批准不冲掉前一次（豁免令牌多枚并存）。
func TestValidateWithTaintReview_ApprovedNodesPass(t *testing.T) {
	a, h, _, vCtx := newTaintReviewFixture(t, true)
	events := a.SubscribeStream(t.Context())

	if err := a.validateWithTaintReview(t.Context(), vCtx); err != nil {
		t.Fatalf("两个节点均获批后应通过校验，got %v", err)
	}
	if len(h.prompts) != 2 {
		t.Fatalf("每个被拦节点应恰好送审一次，got %d", len(h.prompts))
	}
	p := h.prompts[0]
	if p.CheckpointType != types.CheckpointTaintReview || string(p.ExemptionFieldContent) != `{"path":"a.txt"}` || p.TaintLevel < types.TaintMedium {
		t.Fatalf("审批请求须携带节点参数原始字节且 TaintLevel>=Medium（超时一律拒绝），got %+v", p)
	}
	select {
	case ev := <-events:
		if ev.Type != types.AgentStreamEventApproval || ev.Content != p.ID || ev.ToolName != "write_file" {
			t.Fatalf("对话流应收到审批事件，got %+v", ev)
		}
	default:
		t.Fatal("发起审批时未向对话流推送审批事件")
	}
}

// 用户拒绝：返回 L1_taint 错误且原因告知模型勿重试；同一工具+参数本回合不再二次打扰。
func TestValidateWithTaintReview_DeniedNotAskedTwice(t *testing.T) {
	a, h, _, vCtx := newTaintReviewFixture(t, false)
	vCtx.Plan.Nodes = vCtx.Plan.Nodes[:1]

	for range 2 { // 模拟重规划原样产出被拒方案
		err := a.validateWithTaintReview(t.Context(), vCtx)
		ve, ok := err.(*protocol.DAGValidationError)
		if !ok || ve.Layer != "L1_taint" || ve.NodeID != "n1" {
			t.Fatalf("拒绝后应返回 L1_taint 节点错误，got %v", err)
		}
	}
	if len(h.prompts) != 1 {
		t.Fatalf("被拒的同一操作本回合只应询问一次，got %d", len(h.prompts))
	}
}

// 批准后仍被拦（令牌未落库等装配缺陷）不得空转：每节点至多送审一次后按原拦截返回。
func TestValidateWithTaintReview_NoLoopWhenApprovalIneffective(t *testing.T) {
	a, h, v, vCtx := newTaintReviewFixture(t, true)
	h.vault = token.NewExemptionVault() // 铸到另一个 vault，校验侧永远查不到
	vCtx.Plan.Nodes = vCtx.Plan.Nodes[:1]

	if err := a.validateWithTaintReview(t.Context(), vCtx); err == nil {
		t.Fatal("令牌不可查时必须维持拦截")
	}
	if len(h.prompts) != 1 || v.calls != 2 {
		t.Fatalf("应送审 1 次、校验 2 次后收敛，got prompts=%d calls=%d", len(h.prompts), v.calls)
	}
}

// 未装配复核查询器时不发起审批：批准无从生效，询问用户只会制造无效等待。
func TestValidateWithTaintReview_NoCheckerNoPrompt(t *testing.T) {
	a, h, _, vCtx := newTaintReviewFixture(t, true)
	vCtx.ReviewChecker = nil
	a.dagValidator = protocolValidatorFunc(func(context.Context, *protocol.DAGValidationContext) error {
		return &protocol.DAGValidationError{Layer: "L1_taint", NodeID: "n1", Reason: "blocked"}
	})
	if err := a.validateWithTaintReview(t.Context(), vCtx); err == nil {
		t.Fatal("应维持拦截")
	}
	if len(h.prompts) != 0 {
		t.Fatalf("无复核查询器时不应发起审批，got %d", len(h.prompts))
	}
}

type protocolValidatorFunc func(context.Context, *protocol.DAGValidationContext) error

func (f protocolValidatorFunc) Validate(ctx context.Context, v *protocol.DAGValidationContext) error {
	return f(ctx, v)
}
