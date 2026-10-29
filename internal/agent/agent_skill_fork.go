package agent

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// InjectSkillForker 注入 context: fork 技能的委派解析（ADR-0103 决策五）。
func (a *Agent) InjectSkillForker(f SkillForker) { a.skillForker = f }

// tryForkSkill 模型调用 context: fork 技能时，把渲染后的技能正文作为任务委派给技能声明的子 Agent
// （复用 transfer_to_agent 的异步挂起/恢复与 SpawnDepth 门控，不另起执行路径）。
// 子 Agent 自身不能再委派时按普通技能内联执行（Claude：子 Agent 不能派生子 Agent）。
func (a *Agent) tryForkSkill(ctx context.Context, toolName string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, bool, error) {
	if a.skillForker == nil || a.catalog == nil {
		return nil, false, nil
	}
	entry, ok := a.catalog.Lookup(toolName)
	if !ok || entry.Source != types.ToolSkill {
		return nil, false, nil
	}
	if p := a.agentProfile(); p != nil && !p.AllowDelegation {
		return nil, false, nil
	}
	target, fork := a.skillForker.ForkTarget(ctx, entry.SkillName)
	if !fork {
		return nil, false, nil
	}
	// 恢复分支：任务已投递，不重复渲染（渲染会再次执行动态注入命令）。
	if a.sCtx.HandoffTaskID != "" {
		res, err := a.executeTransferToAgent(ctx, target, "", taintLevel)
		return res, true, err
	}
	prompt, err := a.skillForker.RenderSkill(ctx, entry.SkillName, args)
	if err != nil {
		return nil, true, apperr.Wrap(apperr.CodeOf(err), "fork skill "+entry.SkillName, err)
	}
	res, err := a.executeTransferToAgent(ctx, target, prompt, taintLevel)
	return res, true, err
}

// profileToolContext agent 类型 hook 的评估子 Agent 的工具调用不再触发 hooks（防递归）。
func (a *Agent) profileToolContext(ctx context.Context) context.Context {
	if p := a.agentProfile(); p != nil && p.SuppressHooks {
		return context.WithValue(ctx, protocol.CtxHooksSuppressedKey{}, true)
	}
	return ctx
}
