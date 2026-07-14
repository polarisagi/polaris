package tool

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
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
