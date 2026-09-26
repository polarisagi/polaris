package fsm

import (
	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/security/taint"
)

// WriteAgentProfile 子 Agent 运行时把角色指令写入各阶段 prompt（Perceive/Plan/Respond 共用）。
func WriteAgentProfile(b *prompt.PromptBuilder, sCtx *StateContext) {
	sCtx.Mu.RLock()
	p := sCtx.AgentProfile
	sCtx.Mu.RUnlock()
	if p == nil || p.Instructions == "" {
		return
	}
	b.WriteAgentProfile(p.Name, taint.NewTaintedString(p.Instructions,
		taint.TaintSource{Module: "agent_profile", OriginTaintLevel: p.InstructionTaint}, "agent_profile"))
}
