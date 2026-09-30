package main

import (
	"context"

	"github.com/polarisagi/polaris/internal/security/taint"

	"github.com/polarisagi/polaris/internal/action/codeact"
	"github.com/polarisagi/polaris/internal/action/lam"
	"github.com/polarisagi/polaris/internal/agent"
	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/extension/native"
	extskill "github.com/polarisagi/polaris/internal/extension/skill"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── extensionActivatorAdapter ────────────────────────────────────────────────
//
// 将 *native.ExtensionActivator 适配为 agent.ExtensionActivator 接口。
// native.ActivatedHint → fsm.ExtActivatedHint 字段映射。
type extensionActivatorAdapter struct {
	inner *native.ExtensionActivator
}

func (a *extensionActivatorAdapter) FindAndActivate(ctx context.Context, goal string) ([]fsm.ExtActivatedHint, error) {
	hints, err := a.inner.FindAndActivate(ctx, goal)
	if err != nil || hints == nil {
		return nil, err //nolint:wrapcheck
	}
	result := make([]fsm.ExtActivatedHint, 0, len(hints))
	for _, h := range hints {
		result = append(result, fsm.ExtActivatedHint{
			ToolName:    h.ToolName,
			Description: h.Description,
		})
	}
	return result, nil
}

// ─── codeActAdapter ──────────────────────────────────────────────────────────
// 将 *codeact.CodeAct 适配为 agent.CodeActEngine。
// 字段映射：agent.CodeActRequest ↔ protocol.CodeActRequest（两者字段相同，防循环 import 而分别定义）。

type codeActAdapter struct {
	inner *codeact.CodeAct
}

func (a *codeActAdapter) Execute(ctx context.Context, req agent.CodeActRequest) (*agent.CodeActResult, error) {
	result, err := a.inner.Execute(ctx, protocol.CodeActRequest{
		Language:        req.Language,
		Code:            req.Code,
		CapabilityID:    req.CapabilityID,
		SessionID:       req.SessionID,
		AgentID:         req.AgentID,
		StatefulSession: req.StatefulSession,
	})
	if err != nil {
		return nil, err //nolint:wrapcheck
	}
	return &agent.CodeActResult{
		Output:    result.Output,
		ExitCode:  result.ExitCode,
		LatencyMs: result.LatencyMs,
	}, nil
}

func (a *codeActAdapter) IsAvailable() bool { return true }

// ─── skillCacheAdapter ───────────────────────────────────────────────────────
// 将 *skill.ScriptSkillCache 适配为 agent.ScriptSkillCache。
// agent.SkillHandle 仅携带 SkillID，与 skill.ProcessHandle.SkillID 对应。

type skillCacheAdapter struct {
	inner *extskill.ScriptSkillCache
}

func (a *skillCacheAdapter) GetOrSpawn(ctx context.Context, skillID string) (*agent.SkillHandle, error) {
	handle, err := a.inner.GetOrSpawn(ctx, skillID)
	if err != nil || handle == nil {
		return nil, err //nolint:wrapcheck
	}
	return &agent.SkillHandle{SkillID: handle.SkillID}, nil
}

// ─── lamPolicyAdapter ────────────────────────────────────────────────────────
// 将 *lam.ComputerUseEngine 适配为 agent.LAMPolicyChecker。
// agent 只需 CheckPolicy（Cedar 策略预检），完整 ExecuteAction 走 tool/builtin 路径。

type lamPolicyAdapter struct {
	inner *lam.ComputerUseEngine
}

func (a *lamPolicyAdapter) CheckPolicy(ctx context.Context, actionJSON []byte) error {
	return a.inner.CheckPolicy(ctx, actionJSON) //nolint:wrapcheck
}

// ─── agentInvokerAdapter ──────────────────────────────────────────────────────

type agentInvokerAdapter struct {
	agent *agent.Agent
}

func (a *agentInvokerAdapter) InvokeAgent(ctx context.Context, intent string, opts ...any) (string, error) {
	a.agent.SetTaskIntent(taint.NewTaintedString(intent, taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "sys"))
	err := a.agent.SendIntent(types.TriggerIntentReceived)
	return a.agent.AgentID(), err
}
