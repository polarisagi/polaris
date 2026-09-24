package agentctx

import (
	"context"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// writePhaseInstruction 写入阶段契约模板，并在其后追加上下文压力提示。
// 压力提示由 Go 按预算比例生成（进程内常量拼接），与模板同属 TaintNone 指令。
func writePhaseInstruction(b *prompt.PromptBuilder, sCtx *fsm.StateContext, name, fallback string) error {
	fsm.WriteKernelInstruction(b, name, fallback)
	hint := contextPressureHint(sCtx)
	if hint == "" {
		return nil
	}
	safe, err := taint.SanitizeToSafe(taint.NewTaintedString(
		hint, taint.TaintSource{OriginTaintLevel: types.TaintNone}, "context_pressure_hint"))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "writePhaseInstruction: sanitize pressure hint", err)
	}
	b.WriteInstruction(safe)
	return nil
}

// BuildRespondContext 组装 S_RESPOND 的 prompt（ADR-0098 决策二）。
//
// 与 Perceive/Plan 不同，这里不做记忆检索：回复所需事实已在本回合的执行结果与
// 对话历史里，再检索一遍只会把无关旧事件塞进唯一面向用户的输出。ImmutableCore
// （人格 + 用户偏好）与核心记忆保留——它们决定"以谁的口吻、按什么偏好"回答。
func BuildRespondContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *fsm.StateContext) ([]types.Message, error) {
	b := prompt.NewPromptBuilder()
	if memory != nil {
		if blocks, err := memory.ListCoreMemory(ctx, sCtx.AgentID, sCtx.SessionID); err == nil && len(blocks) > 0 {
			b.WriteCoreMemory(blocks)
		}
	}
	fsm.WriteRespondSections(b, sCtx)

	msgs := b.Build()
	if memory != nil {
		msgs = memory.ImmutableCore().PrependToMessages(msgs)
	}
	return fsm.AppendRespondReminder(msgs), nil
}
