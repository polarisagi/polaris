package main

import (
	"context"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/tool/builtin"
)

// mcpA2AListerAdapter 将 *mcp.MCPManager 适配为 builtin.A2AAgentLister（ADR-0084）。
// internal/tool/builtin 属 L1，internal/extension/mcp 属 L2，inv_NoCrossLayerImport
// 禁止 L1 反向 import L2；main 包不受层级限制，在此桥接（与 mcpAsyncTaskAdapter 同一模式）。
type mcpA2AListerAdapter struct {
	inner *mcp.MCPManager
}

func (a *mcpA2AListerAdapter) ListA2AAgents(ctx context.Context) ([]builtin.A2AAgentDescriptor, error) {
	descriptors, err := a.inner.ListA2AAgents(ctx)
	if err != nil {
		return nil, err //nolint:wrapcheck // 纯字段转换适配器，透传底层错误分类
	}
	out := make([]builtin.A2AAgentDescriptor, 0, len(descriptors))
	for _, d := range descriptors {
		out = append(out, builtin.A2AAgentDescriptor{Server: d.Server, Agent: d.Agent, Description: d.Description})
	}
	return out, nil
}
