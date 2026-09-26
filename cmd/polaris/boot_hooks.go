package main

import (
	"context"
	"log/slog"
	"strings"

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
type hookPromptEvaluator struct {
	infer func(ctx context.Context, prompt string, opts ...types.InferOption) (string, error)
}

const hookPromptInstruction = "You are evaluating a lifecycle hook condition. Respond with a single JSON object " +
	`{"ok": true} or {"ok": false, "reason": "<why>"} and nothing else.` + "\n\n"

func (e *hookPromptEvaluator) EvaluateHookPrompt(ctx context.Context, prompt, model string, agent bool) (string, error) {
	opts := []types.InferOption{types.WithPurpose("hook_prompt")}
	if model != "" {
		opts = append(opts, types.WithModel(model))
	}
	_ = agent // agent 类型的只读工具循环由 Agent 画像阶段接入（ADR-0103 决策三）；判定协议相同
	out, err := e.infer(ctx, hookPromptInstruction+prompt, opts...)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeOf(err), "hookPromptEvaluator", err)
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
