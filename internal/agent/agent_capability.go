package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// toolCallKey 工具名 + 参数字节的内容哈希：参数变一个字节即视为另一次调用。
func toolCallKey(toolName string, args []byte) string {
	h := sha256.New()
	h.Write([]byte(toolName))
	h.Write([]byte{0})
	h.Write(args)
	return hex.EncodeToString(h.Sum(nil))
}

// recordValidatedPlan 记下刚通过 S_VALIDATE 的计划，作为本轮 JIT 签发的唯一依据。
// 只收计划节点本身：Saga 补偿动作未经 L1 策略校验，不在其列（fail-closed）。
func (a *Agent) recordValidatedPlan(plan *protocol.DAGPlan) {
	calls := make(map[string]struct{})
	if plan != nil {
		for _, n := range plan.Nodes {
			calls[toolCallKey(n.ToolName, n.Args)] = struct{}{}
		}
	}
	a.validatedCalls.Store(&calls)
}

// withJITCapability 为已通过 S_VALIDATE 的这一次工具调用 JIT 签发能力令牌（M07 §6：
// Gate 全过 → JIT Mint(MaxCalls=1, TTL=5min) → 沙箱执行；ADR-0098 决策五）。
//
// 令牌的含义是"本次调用属于本 Agent 已通过 S_VALIDATE 的计划"：工具名与参数须与校验
// 过的某个节点逐字节一致，否则拒签——执行闸门对写类与 trust<3 工具的令牌要求由此
// 落到可验证的事实上，而不是对任意工具名无条件签发。令牌只注入本次调用的 ctx；执行
// 闸门只 Verify 不 Consume，故由返回的 revoke 在调用结束后撤销，保证一次性。
func (a *Agent) withJITCapability(ctx context.Context, toolName string, args []byte) (context.Context, func(), error) {
	calls := a.validatedCalls.Load()
	if calls == nil {
		return ctx, func() {}, apperr.New(apperr.CodeForbidden, "agent: no validated plan, refusing to mint capability token for "+toolName)
	}
	if _, ok := (*calls)[toolCallKey(toolName, args)]; !ok {
		return ctx, func() {}, apperr.New(apperr.CodeForbidden, "agent: tool call not in validated plan, refusing to mint capability token for "+toolName)
	}
	// sandboxTier 传 0：实际隔离等级由执行闸门 AssignSandboxTier 按信任与工具属性决定。
	tok, err := action.NewJITToken(a.ID, []action.TokenOperation{{ToolName: toolName, MaxCalls: 1}}, 0)
	if err != nil {
		return ctx, func() {}, apperr.Wrap(apperr.CodeForbidden, "agent: JIT capability token mint failed", err)
	}
	revoke := func() { action.GetTokenManager().Revoke(tok.Claims.TokenID) }
	return context.WithValue(ctx, protocol.CtxCapabilityTokenKey{}, tok), revoke, nil
}
