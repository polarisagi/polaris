package main

import (
	"context"
	"log/slog"

	"github.com/polarisagi/polaris/internal/extension/lifecycle"
	"github.com/polarisagi/polaris/internal/extension/mcp"
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
