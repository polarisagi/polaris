package main

import (
	"context"
	"fmt"
	"time"

	"github.com/polarisagi/polaris/internal/action/codeact"
	"github.com/polarisagi/polaris/internal/action/lam"
	"github.com/polarisagi/polaris/internal/agent"
	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/extension/native"
	extskill "github.com/polarisagi/polaris/internal/extension/skill"
	knowledgepkg "github.com/polarisagi/polaris/internal/knowledge"
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
		TaintLevel:      req.TaintLevel,
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
	a.agent.SetTaskIntent([]byte(intent))
	err := a.agent.SendIntent(types.TriggerIntentReceived)
	return a.agent.AgentID(), err
}

// ─── episodicMemAdapter ───────────────────────────────────────────────────────

type episodicMemAdapter struct {
	ep protocol.EpisodicMemory
}

func (a *episodicMemAdapter) Query(ctx context.Context, q string, maxTaint types.TaintLevel) ([]agentctx.ContextItem, error) {
	res, err := a.ep.Query(ctx, types.EpisodicQuery{Semantic: q, MaxTaintLevel: maxTaint, K: 10})
	if err != nil {
		return nil, fmt.Errorf("query episodic memory: %w", err)
	}
	var items []agentctx.ContextItem
	for _, r := range res {
		if ev, ok := r.Event.(*types.Event); ok && ev != nil {
			content := fmt.Sprintf("[%s] %s: %s", ev.CreatedAt.Format(time.RFC3339), ev.Type, string(ev.Payload))
			items = append(items, agentctx.ContextItem{
				Content:   content,
				Source:    "episodic",
				Relevance: r.Score,
				Taint:     ev.TaintLevel,
			})
		}
	}
	return items, nil
}

// ─── knowledgeAdapter ─────────────────────────────────────────────────────────

type knowledgeAdapter struct {
	kb *knowledgepkg.KnowledgeBase
}

func (a *knowledgeAdapter) Search(ctx context.Context, q string, depth int) ([]agentctx.ContextItem, error) {
	if a.kb == nil {
		return nil, nil
	}
	topK := 5
	if depth > 1 {
		topK = 10
	}
	req := knowledgepkg.KnowledgeBaseSearchRequest{
		Query:    q,
		TopK:     topK,
		TaintMax: int(types.TaintHigh),
	}
	res, err := a.kb.Search(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("search knowledge base: %w", err)
	}
	items := make([]agentctx.ContextItem, 0, len(res))
	for _, ac := range res {
		content := ac.Primary.Content
		if ac.Parent != nil {
			content = ac.Parent.Content + "\n" + content
		}
		items = append(items, agentctx.ContextItem{
			Content:   content,
			Source:    "knowledge",
			Relevance: 1.0,
			Taint:     types.TaintLevel(ac.Primary.TaintLevel),
		})
	}
	for i := range items {
		items[i].Relevance = 1.0 / float64(i+1)
	}
	return items, nil
}
