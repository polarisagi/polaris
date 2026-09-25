package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// taintReviewWindow 对话内人工复核的等待窗口。与出口污点豁免令牌 TTL（10 分钟）
// 同量级：批准后本回合立即重新校验并执行，令牌不需要比审批窗口活得更久。
const taintReviewWindow = 10 * time.Minute

// validateWithTaintReview 执行 S_VALIDATE 四层校验；L1_taint 拦截时发起对话内人工复核
// （M11 §2.5 SanitizeByUserReview），批准后重新校验。
//
// 用户输入固定 TaintHigh（ADR-0007），由它派生参数的非只读工具在 L1 必然被拦——这是
// 设计，不是缺陷；缺的是"拦截 → 人审 → 放行"这条既定转义路径没有接到对话里，回合只能
// 以"解释为什么不能做"告终。批准不降低任何防线：放行凭证是按节点参数字节哈希铸造的
// 一次性豁免令牌（HE-2 可验证），参数变一个字节即不匹配，重新送审。
//
// 收敛性：每个节点本回合至多送审一次；批准后同一节点仍被拦（令牌未落库等装配缺陷）
// 按原拦截处理，不空转。
func (a *Agent) validateWithTaintReview(ctx context.Context, vCtx *protocol.DAGValidationContext) error {
	reviewed := make(map[string]bool)
	for {
		err := a.dagValidator.Validate(ctx, vCtx)
		node, ok := taintReviewCandidate(err, vCtx)
		if !ok || a.hitl == nil || vCtx.ReviewChecker == nil || reviewed[node.ID] {
			if err != nil {
				return apperr.Wrap(apperr.CodeForbidden, "s_validate", err)
			}
			return nil
		}
		reviewed[node.ID] = true
		if denyReason := a.requestTaintReview(ctx, node, vCtx.ActiveTaintLevel); denyReason != "" {
			return &protocol.DAGValidationError{Layer: "L1_taint", NodeID: node.ID, Reason: denyReason}
		}
	}
}

// taintReviewCandidate 判定校验错误是否为可经人工复核放行的 L1_taint 节点级拦截。
func taintReviewCandidate(err error, vCtx *protocol.DAGValidationContext) (protocol.ExecNode, bool) {
	var ve *protocol.DAGValidationError
	if err == nil || !errors.As(err, &ve) || ve.Layer != "L1_taint" || ve.NodeID == "" || vCtx.Plan == nil {
		return protocol.ExecNode{}, false
	}
	for _, n := range vCtx.Plan.Nodes {
		if n.ID == ve.NodeID {
			return n, len(n.Args) > 0
		}
	}
	return protocol.ExecNode{}, false
}

// requestTaintReview 就单个节点向用户发起复核，返回空串表示已批准，否则为写入
// 重规划反馈的拒绝原因。同一工具+参数本回合被拒过则不再打扰用户，直接拒绝——
// 重规划可能原样再产出被拒方案，重复弹窗是审批疲劳的直接来源。
func (a *Agent) requestTaintReview(ctx context.Context, node protocol.ExecNode, level types.TaintLevel) string {
	ctx, span := otel.Tracer("agent").Start(ctx, "agent.requestTaintReview")
	defer span.End()
	span.SetAttributes(attribute.String("tool", node.ToolName), attribute.String("node_id", node.ID))

	key := taintReviewKey(node)
	if a.taintReviewDenied[key] {
		span.SetAttributes(attribute.String("decision", "denied_before"))
		return fmt.Sprintf("the user already declined tool %q with these arguments in this turn; do not propose it again, explain to the user instead", node.ToolName)
	}

	riskLevel := types.RiskHigh
	if a.toolRegistry != nil {
		if t, err := a.toolRegistry.Lookup(node.ToolName); err == nil {
			riskLevel = t.RiskLevel
		}
	}
	deadline := time.Now().Add(taintReviewWindow)
	p := types.HITLPrompt{
		ID:             fmt.Sprintf("hitl_%d", time.Now().UnixNano()),
		AgentID:        a.sCtx.AgentID,
		CheckpointType: types.CheckpointTaintReview,
		PromptText: fmt.Sprintf("Tool %q will run with arguments derived from untrusted input (taint=%s). Approve to execute exactly these arguments:\n%s",
			node.ToolName, level.String(), string(node.Args)),
		DeadlineNs: deadline.UnixNano(),
		RiskLevel:  int(riskLevel),
		// 超时兜底据此一律 auto_deny（resolveTimeoutAction：TaintLevel>=Medium 禁止自动放行）。
		TaintLevel:            max(level, types.TaintMedium),
		ExemptionFieldContent: node.Args,
	}
	resp, err := a.promptHITLInTurn(ctx, p, node.ToolName, node.Args)
	switch {
	case err != nil:
		span.SetAttributes(attribute.String("decision", "unavailable"))
		a.markTaintReviewDenied(key)
		return fmt.Sprintf("tool %q requires the user's approval, but the approval was not given in time (%v); explain to the user what was going to be done", node.ToolName, err)
	case resp == nil || !resp.Approved:
		span.SetAttributes(attribute.String("decision", "denied"))
		a.markTaintReviewDenied(key)
		return fmt.Sprintf("the user declined tool %q; do not propose it again, explain to the user instead", node.ToolName)
	default:
		span.SetAttributes(attribute.String("decision", "approved"))
		return ""
	}
}

// promptHITLInTurn 发起阻塞式 HITL 审批，并把审批请求推给当前回合的流订阅方，
// 使对话界面能就地展示审批卡片（此前审批只在自动化页轮询可见，对话里的用户看不到，
// 回合只能挂到超时）。
func (a *Agent) promptHITLInTurn(ctx context.Context, p types.HITLPrompt, toolName string, toolInput []byte) (*types.HITLResponse, error) {
	a.publishStreamEvent(types.AgentStreamEvent{
		Type:       types.AgentStreamEventApproval,
		Content:    p.ID,
		ToolName:   toolName,
		ToolInput:  toolInput,
		DeadlineNs: p.DeadlineNs,
		TaintLevel: p.TaintLevel,
	})
	return a.hitl.Prompt(ctx, p) //nolint:wrapcheck // 调用方按审批结果分支，错误原样透传以保留超时哨兵
}

func (a *Agent) markTaintReviewDenied(key string) {
	if a.taintReviewDenied == nil {
		a.taintReviewDenied = make(map[string]bool)
	}
	a.taintReviewDenied[key] = true
}

// taintReviewKey 工具名 + 参数字节的内容哈希：与豁免令牌同口径，参数一变即视为新请求。
func taintReviewKey(node protocol.ExecNode) string {
	h := sha256.New()
	h.Write([]byte(node.ToolName))
	h.Write([]byte{0})
	h.Write(node.Args)
	return hex.EncodeToString(h.Sum(nil))
}
