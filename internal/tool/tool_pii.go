package tool

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// redactOutputsForPII 是 checkPreExecution 之前 vault.RestoreForTask 的反方向操作
// （2026-07-11 复核修复 GR-6-005；R7 文件行数治理拆分自 tool.go）。
//
// execInput 在真正执行前已被还原为真实 PII 明文传给沙箱/下游工具；如果工具的
// Error/Output 把入参原样回显（例如 CLI 参数校验失败时把命令行打印进 stderr），
// 真实 PII 会经由 ExecuteTool 的返回值泄漏。此前的实现在这里误用了 RestoreForTask
// （token→真实值），而 execErr/execRes 此时已经是真实值而非 token，扫描不到任何
// ⟦PII:xxxx⟧ 模式，等价于 no-op，完全没有起到脱敏效果。
//
// 正确方向是 vault.TokenizeKnownValues（真实值→token），扫描输出中是否包含本次
// 任务命名空间内已知的真实 PII 值并替换回 token，该操作不会失败（找不到匹配就
// 原样返回），因此本函数不再需要返回 error。
func (r *InMemoryToolRegistry) redactOutputsForPII(ctx context.Context, vault *guard.PIITokenVault, execErr error, execRes *sandbox.ExecResult) error {
	taskID, _ := ctx.Value(protocol.CtxTaskIDKey{}).(string)
	if execErr != nil {
		redacted := vault.TokenizeKnownValues(taskID, execErr.Error())
		return apperr.New(apperr.CodeInternal, redacted)
	}
	if execRes == nil {
		return nil
	}
	if len(execRes.Output) > 0 {
		execRes.Output = []byte(vault.TokenizeKnownValues(taskID, string(execRes.Output)))
	}
	if execRes.Error != "" {
		execRes.Error = vault.TokenizeKnownValues(taskID, execRes.Error)
	}
	return nil
}

// restorePIIInput 若 input 携带 PII 令牌（⟦PII:xxxx⟧），在真正执行前原地还原为
// 真实值，仅用于本次调用栈；未携带令牌或 vault 未注入时原样透传；还原失败
// （未知/伪造 token）fail-closed 直接拒绝（从 tool.go ExecuteTool 拆出，
// gocyclo 治理 + R7 文件行数治理，行为不变）。
func (r *InMemoryToolRegistry) restorePIIInput(ctx context.Context, vault *guard.PIITokenVault, input []byte) ([]byte, error) {
	if vault == nil || !vault.HasTokens(string(input)) {
		return input, nil
	}
	taskID, _ := ctx.Value(protocol.CtxTaskIDKey{}).(string)
	restored, restoreErr := vault.RestoreForTask(taskID, string(input))
	if restoreErr != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "tool_registry: PII token restore failed, fail-closed", restoreErr)
	}
	return []byte(restored), nil
}

// finalizeExecError 若 vault 存在则先对错误消息做 PII 二次脱敏，随后判断执行是否失败；
// 返回 handled=true 时调用方应立即返回该 ToolResult（从 tool.go ExecuteTool 拆出，
// gocyclo 治理 + R7 文件行数治理，行为不变）。
func (r *InMemoryToolRegistry) finalizeExecError(ctx context.Context, name string, execErr error, execRes *sandbox.ExecResult, execInput []byte, vault *guard.PIITokenVault, taintLevel types.TaintLevel) (*types.ToolResult, bool) {
	if vault != nil {
		if redacted := r.redactOutputsForPII(ctx, vault, execErr, execRes); redacted != nil {
			execErr = redacted
		}
	}

	if execErr == nil {
		return nil, false
	}
	r.reportOutcome(name, false, 0, execErr.Error(), ctx, execInput, nil)
	return &types.ToolResult{ //nolint:nilerr
		Success:    false,
		Error:      execErr.Error(),
		TaintLevel: taintLevel,
	}, true
}
