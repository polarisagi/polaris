package tool

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// 本文件承载 M04 §3 出口污点检查 + HITL 豁免转义（2026-07-14 补齐；R7 文件
// 行数治理拆分自 tool.go）：policy.Gate.CheckEgressWithExemption 此前完整
// 实现+有测试，但全仓库零生产调用点——agent_execute_dag.go 捕获的
// policy.ErrTaintBlockedEgress 分支因此从未真正触发过，本文件补齐生产方
// （checkTaintEgress 接入 checkPreExecution）。

// TaintEgressChecker 出口污点检查接口（consumer-side 定义，防止 internal/tool
// 反向依赖 internal/security/policy 具体实现；policy.Gate 满足此接口）。
type TaintEgressChecker interface {
	CheckEgressWithExemption(data []byte, taintLevel types.TaintLevel, tok *token.TaintExemptionToken) error
}

// WithTaintEgressChecker 注入出口污点检查器（可选）。注入后，ExecuteTool 对
// 声明 SideNetworkCall 副作用且入参 TaintLevel>=TaintMedium 的工具强制检查，
// 未持有效豁免令牌时拒绝执行。
func (r *InMemoryToolRegistry) WithTaintEgressChecker(c TaintEgressChecker) *InMemoryToolRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.taintEgressChecker = c
	return r
}

// WithExemptionVault 注入 HITL 豁免令牌存储（可选，与 WithTaintEgressChecker
// 配套注入；nil 时出口污点检查永远查不到豁免令牌，等价于只挡不放）。
func (r *InMemoryToolRegistry) WithExemptionVault(v *token.ExemptionVault) *InMemoryToolRegistry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.exemptionVault = v
	return r
}

// checkTaintEgress 出口污点检查（M04 §3 + M11 §2.3/§6）：具备 SideNetworkCall
// 副作用的工具，若本次调用入参 TaintLevel >= TaintMedium（含 URL/查询参数本身
// 由上游被污染内容衍生的场景，防 prompt-injection 诱导 exfiltration），必须
// 持有匹配的 HITL 豁免令牌才能放行——否则拒绝，返回的错误经 apperr.Wrap 后
// 仍可通过 errors.Is(err, policy.ErrTaintBlockedEgress) 命中
// （*policy.TaintEgressBlockedError.Unwrap() 指向该哨兵），由上游 runExecuteDAG
// 捕获后发起 HITL 转义审批。r.taintEgressChecker 为 nil（未注入）时跳过，行为
// 与改造前完全一致。
func (r *InMemoryToolRegistry) checkTaintEgress(ctx context.Context, tool types.Tool, taintLevel types.TaintLevel, input []byte) error {
	if r.taintEgressChecker == nil || taintLevel < types.TaintMedium || !hasNetworkEgressSideEffect(tool) {
		return nil
	}
	var exemption *token.TaintExemptionToken
	if r.exemptionVault != nil {
		if agentID, ok := ctx.Value(protocol.CtxAgentIDKey{}).(string); ok && agentID != "" {
			exemption = r.exemptionVault.Lookup(agentID)
		}
	}
	if err := r.taintEgressChecker.CheckEgressWithExemption(input, taintLevel, exemption); err != nil {
		return apperr.Wrap(apperr.CodeForbidden, "tool_registry: taint egress blocked", err)
	}
	return nil
}
