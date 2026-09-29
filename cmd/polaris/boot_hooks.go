package main

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// mcpHookCaller 实现 hook.MCPToolCaller：mcp_tool 处理器按服务器名定位连接。
// Claude 插件 hook 以 "plugin:<插件>:<服务器>" 引用本插件服务器，对应行名 "<插件>-<服务器>"。
type mcpHookCaller struct{ mgr *mcp.MCPManager }

func (c *mcpHookCaller) CallHookTool(ctx context.Context, server, tool string, args map[string]any) (string, error) {
	want := server
	if rest, ok := strings.CutPrefix(server, "plugin:"); ok {
		if plugin, srv, found := strings.Cut(rest, ":"); found {
			want = plugin + "-" + srv
			if plugin == srv {
				want = plugin
			}
		}
	}
	for _, info := range c.mgr.ListServers() {
		if info.Name == want || info.ID == server {
			out, err := c.mgr.CallTool(ctx, info.ID, tool, args)
			if err != nil {
				return "", apperr.Wrap(apperr.CodeOf(err), "mcpHookCaller", err)
			}
			return out, nil
		}
	}
	return "", apperr.New(apperr.CodeNotFound, "hook: mcp server not connected: "+server)
}

// hookPromptEvaluator 实现 hook.PromptEvaluator：经 Provider 路由（R1.11）。判定是机械性任务，
// 走便宜档并关闭思考（ADR-0101）；hook 声明的 model 以模型池/ID 覆盖由路由处理。
// agent 类型经子 Agent 执行器以只读角色运行（多轮工具调用由内核 FSM 驱动，HE-5）。
type hookPromptEvaluator struct {
	infer     func(ctx context.Context, prompt string, opts ...types.InferOption) (string, error)
	subagents atomic.Pointer[orchestrator.SubagentRunner]
}

const hookPromptInstruction = "You are evaluating a lifecycle hook condition. Respond with a single JSON object " +
	`{"ok": true} or {"ok": false, "reason": "<why>"} and nothing else.` + "\n\n"

// bindSubagents Agent 池在 hooks 引擎之后构造，启动装配完成时绑定。
func (e *hookPromptEvaluator) bindSubagents(r *orchestrator.SubagentRunner) { e.subagents.Store(r) }

func (e *hookPromptEvaluator) EvaluateHookPrompt(ctx context.Context, prompt, model string, agent bool) (string, error) {
	if agent {
		return e.evaluateWithAgent(ctx, prompt)
	}
	opts := []types.InferOption{types.WithPurpose("hook_prompt")}
	if model != "" {
		opts = append(opts, types.WithModel(model))
	}
	out, err := e.infer(ctx, hookPromptInstruction+prompt, opts...)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeOf(err), "hookPromptEvaluator", err)
	}
	return out, nil
}

// hookAgentProfile agent 类型 hook（Claude）：只读工具（Read/Grep/Glob）多轮核查，最多 50 轮；
// 其工具调用不再触发 hooks，否则 PreToolUse agent hook 会无限递归。
func hookAgentProfile() *types.AgentProfileSpec {
	return &types.AgentProfileSpec{Name: "hook-agent", Source: "builtin",
		Instructions: "You verify a lifecycle hook condition for the host agent. Inspect files with the read-only tools " +
			"as needed, then answer with the required JSON object only.",
		InstructionTaint: types.TaintLow, Tools: []string{"Read", "Grep", "Glob", "LS"},
		ReadOnly: true, MaxTurns: 50, SuppressHooks: true}
}

func (e *hookPromptEvaluator) evaluateWithAgent(ctx context.Context, prompt string) (string, error) {
	r := e.subagents.Load()
	if r == nil {
		return "", apperr.New(apperr.CodeInternal, "agent hook: subagent runner not ready")
	}
	out, err := r.Run(ctx, orchestrator.SubagentRequest{Profile: hookAgentProfile(), Prompt: hookPromptInstruction + prompt,
		Entry: orchestrator.SubagentEntryHook})
	if err != nil {
		return "", apperr.Wrap(apperr.CodeOf(err), "agent hook", err)
	}
	return out, nil
}

// activeProjectDirs 未归档且绑定目录的项目根（项目级 .polaris/hooks/hooks.json 所在位置）。
func activeProjectDirs(projectRepo *repo.SQLiteProjectRepository) []string {
	projects, err := projectRepo.ListProjects(context.Background(), false)
	if err != nil {
		slog.Warn("hook: list projects failed, project-level hooks skipped", "err", err)
		return nil
	}
	var dirs []string
	for _, p := range projects {
		if p.RootPath != "" {
			dirs = append(dirs, p.RootPath)
		}
	}
	return dirs
}
