package main

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/gateway/session"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/builtin"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// registerDelegationTools 组装子 Agent 定义来源并注册委派工具。技能预加载经技能执行器
// （与模型调用技能同一渲染/动态注入审阅路径）。
func registerDelegationTools(sbx *sandbox.InProcessSandbox, toolReg *tool.InMemoryToolRegistry, extRepo *repo.SQLiteExtensionRepository,
	dataDir string, projectRepo *repo.SQLiteProjectRepository, mcpMgr *mcp.MCPManager, skills protocol.SkillExecutor,
) *lifecycle.AgentDefinitionProvider {
	defs := lifecycle.NewAgentDefinitionProvider(extRepo, dataDir, func() []string { return activeProjectDirs(projectRepo) },
		func(ctx context.Context, name string) (string, error) {
			out, err := skills.ExecuteSkill(ctx, name, nil)
			if err != nil {
				return "", apperr.Wrap(apperr.CodeOf(err), "preload skill", err)
			}
			return string(out), nil
		})
	if err := builtin.RegisterDelegationTools(sbx, toolReg, localAgentLister{defs}, &mcpA2AListerAdapter{inner: mcpMgr}); err != nil {
		slog.Warn("polaris: delegation tool registration failed", "err", err)
	}
	return defs
}

// localAgentLister 将 lifecycle.AgentDefinitionProvider 适配为 builtin.LocalAgentLister（L1 不 import L2）。
type localAgentLister struct {
	defs *lifecycle.AgentDefinitionProvider
}

func (l localAgentLister) ListLocalAgents(ctx context.Context) ([]builtin.LocalAgentDescriptor, error) {
	defs, _, err := l.defs.ListAgentDefinitions(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "localAgentLister", err)
	}
	out := make([]builtin.LocalAgentDescriptor, 0, len(defs))
	for _, d := range defs {
		out = append(out, builtin.LocalAgentDescriptor{Name: d.Name, Description: d.Description, Source: d.Source})
	}
	return out, nil
}

// skillForker 实现 agent.SkillForker：技能 spec（pluginspec.Skill JSON）声明 context: fork 时
// 委派给其 agent 字段指定的子 Agent（缺省 general-purpose，Claude 规则）。
type skillForker struct {
	reg  protocol.SkillRegistry
	exec protocol.SkillExecutor
}

func (f skillForker) ForkTarget(ctx context.Context, skillName string) (string, bool) {
	meta, err := f.reg.Get(ctx, skillName, "")
	if err != nil || meta == nil || meta.Spec == "" {
		return "", false
	}
	var spec struct {
		Context string `json:"context"`
		Agent   string `json:"agent"`
	}
	if err := json.Unmarshal([]byte(meta.Spec), &spec); err != nil {
		slog.Warn("skill: corrupt spec, fork context ignored", "skill", skillName, "err", err)
		return "", false
	}
	if spec.Context != "fork" {
		return "", false
	}
	if spec.Agent == "" {
		spec.Agent = builtin.GeneralPurposeAgent
	}
	return spec.Agent, true
}

func (f skillForker) RenderSkill(ctx context.Context, skillName string, args []byte) (string, error) {
	out, err := f.exec.ExecuteSkill(ctx, skillName, args)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeOf(err), "skillForker", err)
	}
	return string(out), nil
}

// newSubagentRunner 组装子 Agent 执行器：定义解析来自 AgentDefinitionProvider，生命周期 hook
// 来自 hooks.json 引擎；并把 agent 类型 hook 的评估接到同一执行器（引擎先于 Agent 池构造）。
func newSubagentRunner(pool protocol.AgentPool, tb *ToolBundle) *orchestrator.SubagentRunner {
	var profiles orchestrator.AgentProfileResolver
	if tb.AgentDefs != nil {
		profiles = tb.AgentDefs
	}
	var hooks orchestrator.SubagentHooks
	if tb.HookRunner != nil {
		hooks = tb.HookRunner
	}
	runner := orchestrator.NewSubagentRunner(pool, profiles, hooks)
	if tb.HookEvaluator != nil {
		tb.HookEvaluator.bindSubagents(runner)
	}
	return runner
}

// channelTurnRunner 实现 lifecycle.ChannelTurnRunner：channel 事件作为该 channel 专属会话的一轮
// headless 对话（与聊天平台 channel 同一会话编排路径）。回复经模型调用服务器的回复工具送出，
// 宿主不回发最终文本（Claude channel 语义）。
type channelTurnRunner struct{ orch session.Orchestrator }

func (r channelTurnRunner) RunChannelTurn(ctx context.Context, sessionID, input string) error {
	res, err := r.orch.RunTurn(ctx, session.Request{SessionID: sessionID, Input: input, Channel: "plugin_channel",
		Headless: true, TitleHint: sessionID}, session.NewBufferSink())
	if err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "channelTurnRunner", err)
	}
	if res != nil && res.Aborted {
		return apperr.New(apperr.CodeCancelled, "channel turn aborted")
	}
	return nil
}
