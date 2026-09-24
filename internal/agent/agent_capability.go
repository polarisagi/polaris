package agent

import (
	"context"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// withJITCapability 为已通过 S_VALIDATE 的这一次工具调用 JIT 签发能力令牌（M07 §6：
// Gate 全过 → JIT Mint(MaxCalls=1, TTL=5min) → 沙箱执行；ADR-0098 决策五）。
//
// 此前通用工具路径从不签发，执行闸门对写类工具（Step 3）与 trust<3 工具
// （tool_execute_permit）一律拒绝。令牌只注入本次调用的 ctx；执行闸门只 Verify
// 不 Consume，故由返回的 revoke 在调用结束后撤销，保证一次性、不可被后续节点复用。
func (a *Agent) withJITCapability(ctx context.Context, toolName string) (context.Context, func(), error) {
	// sandboxTier 传 0：实际隔离等级由执行闸门 AssignSandboxTier 按信任与工具属性决定。
	tok, err := action.NewJITToken(a.ID, []action.TokenOperation{{ToolName: toolName, MaxCalls: 1}}, 0)
	if err != nil {
		return ctx, func() {}, apperr.Wrap(apperr.CodeForbidden, "agent: JIT capability token mint failed", err)
	}
	revoke := func() { action.GetTokenManager().Revoke(tok.Claims.TokenID) }
	return context.WithValue(ctx, protocol.CtxCapabilityTokenKey{}, tok), revoke, nil
}
